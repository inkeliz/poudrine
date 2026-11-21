package poudrine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"regexp"
	"strings"
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
func upgradeReportActivity(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Both the build worker and the repository can open the database for writing.
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
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
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

const (
	reportPath     = "/api/inventory"
	maxReportBytes = 8 << 20
	maxReportLines = 65536
)

var reportToken = regexp.MustCompile(`^[-A-Za-z0-9_+.,:~]+$`)
var reportOrigin = regexp.MustCompile(`^[-A-Za-z0-9_+./:@~]+$`)
var errReportRows = errors.New("inventory exceeds 65536 packages")
var errReportKey = errors.New("the report key does not match the key registered for this client address")
var errReportReplay = errors.New("this report nonce has already been used")
var errReportExpired = errors.New("the inventory report signature expired")
var reportOriginProtection = http.NewCrossOriginProtection()

type installedPackage struct {
	version, origin, buildOrigin string
}

type inventoryReceipt struct {
	Reported  int `json:"reported"`
	Matched   int `json:"matched"`
	Unmatched int `json:"unmatched"`
}

func parseReport(body io.Reader, catalog *catalogSnapshot) (map[string]installedPackage, inventoryReceipt, error) {
	packages := make(map[string]installedPackage)
	var result inventoryReceipt
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024), 1024)
	line := 0
	for scanner.Scan() {
		// A reader error can leave a partial final line. Propagate the
		// request size limit before interpreting that fragment as a record.
		if err := scanner.Err(); err != nil {
			return nil, result, err
		}
		line++
		if line > maxReportLines {
			return nil, result, errReportRows
		}
		fields := strings.FieldsFunc(scanner.Text(), func(r rune) bool { return r == ' ' || r == '\t' })
		if len(fields) != 3 {
			return nil, result, fmt.Errorf("line %d: expected package name, installed version and origin", line)
		}
		name, version, origin := fields[0], fields[1], fields[2]
		if len(name) > 255 || len(version) > 255 || len(origin) > 255 ||
			!reportToken.MatchString(name) || !reportToken.MatchString(version) || !reportOrigin.MatchString(origin) {
			return nil, result, fmt.Errorf("line %d: invalid package name, version or origin", line)
		}
		if old, ok := packages[name]; ok {
			if old.version != version || old.origin != origin {
				return nil, result, fmt.Errorf("line %d: conflicting entries for the same package", line)
			}
			continue
		}
		installed := installedPackage{version: version, origin: origin}
		// Only the current catalog supplies a Poudriere build identifier.
		// Unknown packages and pkgbase entries still belong in the client list.
		if pkg, ok := catalog.byPath[catalog.pathByName[name]]; ok && pkg.Origin == origin && !strings.HasPrefix(origin, "base/") {
			if len(pkg.BuildOrigin) <= 255 && buildOrigin.MatchString(pkg.BuildOrigin) {
				installed.buildOrigin = pkg.BuildOrigin
				result.Matched++
			}
		}
		packages[name] = installed
	}
	result.Reported = len(packages)
	result.Unmatched = result.Reported - result.Matched
	return packages, result, scanner.Err()
}

type reportProof struct {
	key   ed25519.PublicKey
	nonce []byte
	stamp int64
}

func (s inventory) replaceReport(ctx context.Context, ip string, proof reportProof, packages map[string]installedPackage) error {
	tx, err := s.db.BeginTx(ctx, nil)
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
	if _, err := tx.ExecContext(ctx, "INSERT INTO client_report_keys(ip,public_key) VALUES(?,?) ON CONFLICT(ip) DO NOTHING", ip, []byte(proof.key)); err != nil {
		return err
	}
	var pin []byte
	if err := tx.QueryRowContext(ctx, "SELECT public_key FROM client_report_keys WHERE ip=?", ip).Scan(&pin); err != nil {
		return err
	}
	if !bytes.Equal(pin, proof.key) {
		return errReportKey
	}
	// Parsing a large report or waiting for another writer can consume the
	// rest of its validity window. Check again while holding the write lock.
	expires := proof.stamp + requestauth.ClockSkewSeconds
	now = time.Now().Unix()
	if expires < now {
		return errReportExpired
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM client_report_nonces WHERE expires < ?", now); err != nil {
		return err
	}
	used, err := tx.ExecContext(ctx, "INSERT INTO client_report_nonces(nonce,expires) VALUES(?,?) ON CONFLICT(nonce) DO NOTHING", proof.nonce, expires)
	if err != nil {
		return err
	}
	n, err := used.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errReportReplay
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
		if _, err = insert.ExecContext(ctx, ip, name, p.version, p.origin, p.buildOrigin); err != nil {
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

func (s *repository) reportInventory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RequestURI() != reportPath {
		http.Error(w, "use the inventory path without URL parameters", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "use POST to report installed packages", http.StatusMethodNotAllowed)
		return
	}
	if reportOriginProtection.Check(r) != nil {
		http.Error(w, "cross-origin inventory reports are forbidden", http.StatusForbidden)
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "text/plain" {
		http.Error(w, "send pkg query output as text/plain", http.StatusUnsupportedMediaType)
		return
	}
	key := requestauth.PublicKey(r.Header.Get("X-Poudrine-Key"))
	if key == nil || r.Header.Get("X-Poudrine-Signature") == "" {
		http.Error(w, "a signed Ed25519 inventory report is required", http.StatusUnauthorized)
		return
	}
	if r.ContentLength > maxReportBytes {
		http.Error(w, "inventory exceeds 8 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReportBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "inventory exceeds 8 MiB", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "cannot read the inventory report", http.StatusBadRequest)
		}
		return
	}
	nonce, stamp, valid := requestauth.Verify(key, r.Method, reportPath, r.Header.Get("X-Poudrine-Time"),
		r.Header.Get("X-Poudrine-Nonce"), r.Header.Get("X-Poudrine-Signature"), body)
	if !valid {
		http.Error(w, "invalid inventory report signature", http.StatusUnauthorized)
		return
	}
	snapshot := s.catalog.current.Load()
	if snapshot == nil {
		http.Error(w, "repository catalog is initializing; retry the report when it is ready", http.StatusServiceUnavailable)
		return
	}
	packages, result, err := parseReport(bytes.NewReader(body), snapshot)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errReportRows) {
			code = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), code)
		return
	}
	if err := s.store.replaceReport(r.Context(), s.clientIP(r), reportProof{key, nonce, stamp}, packages); err != nil {
		switch {
		case errors.Is(err, errReportKey):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, errReportReplay):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errReportExpired):
			http.Error(w, err.Error(), http.StatusUnauthorized)
		default:
			log.Printf("record installed packages: %v", err)
			http.Error(w, "inventory unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("inventory report response: %v", err)
	}
}
