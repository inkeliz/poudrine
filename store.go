package poudrine

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

func openInventory(dir string, readOnly bool) (*sql.DB, error) {
	path, err := filepath.Abs(filepath.Join(dir, "inventory.sqlite"))
	if err != nil {
		return nil, err
	}
	mode := "rwc"
	if readOnly {
		mode = "ro"
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(5000)", "foreign_keys(1)", "trusted_schema(0)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if readOnly {
		err = db.Ping()
	} else {
		_, err = db.Exec(`
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS clients (
 ip TEXT PRIMARY KEY, first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tracked_packages (
 name TEXT PRIMARY KEY, origin TEXT NOT NULL, build_origin TEXT NOT NULL, first_seen INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS package_requests (
 ip TEXT NOT NULL REFERENCES clients(ip), name TEXT NOT NULL REFERENCES tracked_packages(name),
 version TEXT NOT NULL, last_seen INTEGER NOT NULL, PRIMARY KEY(ip,name)
);
CREATE INDEX IF NOT EXISTS package_requests_name ON package_requests(name);` + reportSchema + buildHistorySchema)
		if err == nil {
			err = upgradeReportActivity(db)
		}
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type inventory struct{ db *sql.DB }

const recordClientSQL = `INSERT INTO clients(ip,first_seen,last_seen) VALUES(?,?,?)
 ON CONFLICT(ip) DO UPDATE SET last_seen=max(last_seen,excluded.last_seen)`

const trackPackageSQL = `INSERT INTO tracked_packages(name,origin,build_origin,first_seen) VALUES(?,?,?,?)
 ON CONFLICT(name) DO UPDATE SET origin=excluded.origin,build_origin=excluded.build_origin`

func (s inventory) record(ctx context.Context, ip string, p *CatalogPackage) error {
	tx, err := s.db.BeginTx(ctx, nil)
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

func (s inventory) origins(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT build_origin FROM tracked_packages ORDER BY build_origin")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type clientRecord struct {
	IP               string   `json:"ip"`
	FirstSeen        int64    `json:"first_seen"`
	LastSeen         int64    `json:"last_seen"`
	Packages         []string `json:"packages"`
	ReportedPackages []string `json:"reported_packages"`
	ReportedAt       int64    `json:"reported_at,omitempty"`
}

func (s inventory) clients(ctx context.Context) (any, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.ip,c.first_seen,c.last_seen,coalesce(r.reported_at,0),p.name,p.version,coalesce(p.reported,0)
FROM clients c
LEFT JOIN client_reports r ON c.ip=r.ip
LEFT JOIN (
 SELECT ip,name,version,0 AS reported FROM package_requests
 UNION ALL SELECT ip,name,version,1 FROM client_report_packages
) p ON c.ip=p.ip
ORDER BY c.ip,p.reported,p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []clientRecord{}
	for rows.Next() {
		var c clientRecord
		var name, version sql.NullString
		var reported bool
		if err := rows.Scan(&c.IP, &c.FirstSeen, &c.LastSeen, &c.ReportedAt, &name, &version, &reported); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].IP != c.IP {
			c.Packages = []string{}
			c.ReportedPackages = []string{}
			out = append(out, c)
		}
		if name.Valid {
			i := len(out) - 1
			label := name.String + "-" + version.String
			if reported {
				out[i].ReportedPackages = append(out[i].ReportedPackages, label)
			} else {
				out[i].Packages = append(out[i].Packages, label)
			}
		}
	}
	return out, rows.Err()
}

type trackedPackageRecord struct {
	Name        string `json:"name"`
	Origin      string `json:"origin"`
	BuildOrigin string `json:"build_origin"`
	FirstSeen   int64  `json:"first_seen"`
	Requested   bool   `json:"requested"`
	Reported    bool   `json:"reported"`
	LastSeen    int64  `json:"last_seen"`
	Consumers   int    `json:"consumers"`
}

func (s inventory) tracked(ctx context.Context) (any, error) {
	return s.trackedPackages(ctx)
}

func (s inventory) trackedPackages(ctx context.Context) ([]trackedPackageRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH consumers AS (
 SELECT name,ip,last_seen,1 AS requested FROM package_requests
 UNION ALL
 SELECT p.name,p.ip,r.reported_at,0
 FROM client_report_packages p JOIN client_reports r ON r.ip=p.ip
 WHERE p.build_origin<>''
), usage AS (
 SELECT name,count(DISTINCT ip) AS consumers,max(last_seen) AS last_seen,max(requested) AS requested
 FROM consumers GROUP BY name
)
SELECT t.name,t.origin,t.build_origin,t.first_seen,coalesce(u.requested,0),r.name IS NOT NULL,
 max(t.first_seen,coalesce(u.last_seen,0),coalesce(r.last_seen,0)),coalesce(u.consumers,0)
FROM tracked_packages t
LEFT JOIN usage u ON u.name=t.name
LEFT JOIN reported_targets r ON r.name=t.name
ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []trackedPackageRecord{}
	for rows.Next() {
		var p trackedPackageRecord
		if err := rows.Scan(&p.Name, &p.Origin, &p.BuildOrigin, &p.FirstSeen, &p.Requested, &p.Reported, &p.LastSeen, &p.Consumers); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

var errUntrustedPackage = errors.New("package contents do not match the signed catalog")
