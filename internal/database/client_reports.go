package database

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"

	"github.com/inkeliz/poudrine/internal/requestauth"
)

// Installed packages are a snapshot per client. Catalog-matched targets are
// retained independently, just like previously downloaded ports.
const reportSchema = `
CREATE TABLE IF NOT EXISTS client_reports (
 ip TEXT PRIMARY KEY REFERENCES clients(ip), reported_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS client_report_packages (
 ip TEXT NOT NULL REFERENCES client_reports(ip), name TEXT NOT NULL, version TEXT NOT NULL,
 origin TEXT NOT NULL, build_origin TEXT NOT NULL, PRIMARY KEY(ip,name)
);
CREATE TABLE IF NOT EXISTS client_report_keys (
 ip TEXT PRIMARY KEY REFERENCES clients(ip), public_key BLOB NOT NULL CHECK(length(public_key)=32)
);
CREATE TABLE IF NOT EXISTS client_report_nonces (
 nonce BLOB PRIMARY KEY, expires INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS client_report_nonce_expiry ON client_report_nonces(expires);
CREATE TABLE IF NOT EXISTS reported_targets (
 name TEXT PRIMARY KEY REFERENCES tracked_packages(name), last_seen INTEGER NOT NULL DEFAULT 0
);`

// Older databases only recorded whether a package had ever been reported.
// Preserve the newest available report timestamp before a client replaces it.
func upgradeReportActivity(ctx context.Context, conn *sql.Conn) error {
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_table_info('reported_targets') WHERE name='last_seen')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		_, err := conn.ExecContext(ctx, `
ALTER TABLE reported_targets ADD COLUMN last_seen INTEGER NOT NULL DEFAULT 0;
WITH latest AS (
 SELECT p.name,max(r.reported_at) AS seen FROM client_report_packages p
 JOIN client_reports r ON r.ip=p.ip WHERE p.build_origin<>'' GROUP BY p.name
)
UPDATE reported_targets AS t SET last_seen=l.seen FROM latest AS l WHERE t.name=l.name;`)
		if err != nil {
			return err
		}
	}
	return nil
}

var ErrReportKey = errors.New("the report key does not match the key registered for this client address")

var ErrReportReplay = errors.New("this report nonce has already been used")

var ErrReportExpired = errors.New("the inventory report signature expired")

type InstalledPackage struct {
	Version, Origin, BuildOrigin string `json:"-"`
}

func (s DB) ReplaceReport(ctx context.Context, ip string, proof requestauth.ReportProof, packages map[string]InstalledPackage) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err = tx.ExecContext(ctx, recordClientSQL, ip, now, now); err != nil {
		return err
	}
	// Pin the first accepted key for this address. Share the write transaction
	// with the nonce and inventory so concurrent first reports cannot both win.
	if _, err := tx.ExecContext(ctx, "INSERT INTO client_report_keys(ip,public_key) VALUES(?,?) ON CONFLICT(ip) DO NOTHING", ip, []byte(proof.Key)); err != nil {
		return err
	}
	var pin []byte
	if err := tx.QueryRowContext(ctx, "SELECT public_key FROM client_report_keys WHERE ip=?", ip).Scan(&pin); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(pin, proof.Key) != 1 {
		return ErrReportKey
	}
	// Parsing a large report or waiting for another writer can consume the
	// rest of its validity window. Check again while holding the write lock.
	expires := proof.Stamp + requestauth.ClockSkewSeconds
	now = time.Now().Unix()
	if expires < now {
		return ErrReportExpired
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM client_report_nonces WHERE expires < ?", now); err != nil {
		return err
	}
	used, err := tx.ExecContext(ctx, "INSERT INTO client_report_nonces(nonce,expires) VALUES(?,?) ON CONFLICT(nonce) DO NOTHING", proof.Nonce, expires)
	if err != nil {
		return err
	}
	n, err := used.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrReportReplay
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO client_reports(ip,reported_at) VALUES(?,?)
		ON CONFLICT(ip) DO UPDATE SET reported_at=excluded.reported_at`, ip, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM client_report_packages WHERE ip=?", ip); err != nil {
		return err
	}
	insert, err := tx.PrepareContext(ctx, `INSERT INTO client_report_packages(ip,name,version,origin,build_origin) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	for name, p := range packages {
		if _, err = insert.ExecContext(ctx, ip, name, p.Version, p.Origin, p.BuildOrigin); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tracked_packages(name,origin,build_origin,first_seen)
		SELECT name,origin,build_origin,? FROM client_report_packages WHERE ip=? AND build_origin<>''
		ON CONFLICT(name) DO UPDATE SET origin=excluded.origin, build_origin=excluded.build_origin`, now, ip); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO reported_targets(name,last_seen)
		SELECT name,? FROM client_report_packages WHERE ip=? AND build_origin<>''
		ON CONFLICT(name) DO UPDATE SET last_seen=max(last_seen,excluded.last_seen)`, now, ip); err != nil {
		return err
	}
	return tx.Commit()
}
