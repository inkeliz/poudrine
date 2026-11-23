package database

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/inkeliz/poudrine/internal/packageinfo"
)

const recordClientSQL = `INSERT INTO clients(ip,first_seen,last_seen) VALUES(?,?,?)
 ON CONFLICT(ip) DO UPDATE SET last_seen=max(last_seen,excluded.last_seen)`

func (s DB) Record(ctx context.Context, ip string, p *packageinfo.Package) error {
	if !validClientIP(ip) {
		return errors.New("invalid client address")
	}
	if p != nil {
		origin := p.BuildOrigin
		if origin == "" {
			origin = p.Origin
		}
		if !packageinfo.ValidInventoryPackage(p.Name, p.Version, p.Origin) || len(origin) > 255 || !packageinfo.BuildOrigin.MatchString(origin) {
			return errors.New("invalid requested package")
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Unix()
	_, err = tx.ExecContext(ctx, recordClientSQL, ip, now, now)
	if err != nil {
		return err
	}
	if p != nil {
		origin := p.BuildOrigin
		if origin == "" {
			origin = p.Origin
		}
		_, err = tx.ExecContext(ctx, trackPackageSQL, p.Name, p.Origin, origin, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO package_requests VALUES(?,?,?,?) ON CONFLICT(ip,name) DO UPDATE SET version=excluded.version, last_seen=excluded.last_seen`, ip, p.Name, p.Version, now)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

const packageRequestsSchema = `CREATE TABLE IF NOT EXISTS package_requests (
 ip TEXT NOT NULL REFERENCES clients(ip), name TEXT NOT NULL REFERENCES tracked_packages(name),
 version TEXT NOT NULL, last_seen INTEGER NOT NULL, PRIMARY KEY(ip,name)
);
CREATE INDEX IF NOT EXISTS package_requests_name ON package_requests(name);`

func validClientIP(ip string) bool {
	if len(ip) > 64 {
		return false
	}
	_, err := netip.ParseAddr(ip)
	return err == nil
}
