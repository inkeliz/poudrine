package database

import (
	"context"
)

const trackPackageSQL = `INSERT INTO tracked_packages(name,origin,build_origin,first_seen) VALUES(?,?,?,?)
 ON CONFLICT(name) DO UPDATE SET origin=excluded.origin,build_origin=excluded.build_origin`

func (s DB) Origins(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT DISTINCT build_origin FROM tracked_packages ORDER BY build_origin")
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

type TrackedPackageRecord struct {
	Name        string `json:"name"`
	Origin      string `json:"origin"`
	BuildOrigin string `json:"build_origin"`
	FirstSeen   int64  `json:"first_seen"`
	Requested   bool   `json:"requested"`
	Reported    bool   `json:"reported"`
	LastSeen    int64  `json:"last_seen"`
	Consumers   int    `json:"consumers"`
}

func (s DB) Tracked(ctx context.Context) ([]TrackedPackageRecord, error) {
	return s.TrackedPackages(ctx)
}

func (s DB) TrackedPackages(ctx context.Context) ([]TrackedPackageRecord, error) {
	rows, err := s.DB.QueryContext(ctx, `
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
	out := []TrackedPackageRecord{}
	for rows.Next() {
		var p TrackedPackageRecord
		if err := rows.Scan(&p.Name, &p.Origin, &p.BuildOrigin, &p.FirstSeen, &p.Requested, &p.Reported, &p.LastSeen, &p.Consumers); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const trackedPackagesSchema = `CREATE TABLE IF NOT EXISTS tracked_packages (
 name TEXT PRIMARY KEY, origin TEXT NOT NULL, build_origin TEXT NOT NULL, first_seen INTEGER NOT NULL
);
`
