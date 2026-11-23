package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type ClientSummary struct {
	IP           string `json:"ip"`
	FirstSeen    int64  `json:"first_seen"`
	LastSeen     int64  `json:"last_seen"`
	PackageCount int    `json:"package_count"`
}

type ClientPackageDetail struct {
	Name             string `json:"name"`
	Origin           string `json:"origin"`
	RequestedVersion string `json:"requested_version,omitempty"`
	ReportedVersion  string `json:"reported_version,omitempty"`
}

type ClientPackageList struct {
	ClientSummary
	ReportedAt     int64                 `json:"reported_at,omitempty"`
	PackageDetails []ClientPackageDetail `json:"package_details"`
}

const ClientPageSize = 25
const ClientActivityRetention = 180 * 24 * time.Hour

var ErrClientPage = errors.New("invalid client page")

type ClientPageQuery struct {
	Offset int    `json:"offset"`
	Search string `json:"search"`
}

type ClientSummaryPage struct {
	Clients []ClientSummary `json:"clients"`
	More    bool            `json:"more"`
}

func clientPageQuery(queries []ClientPageQuery) (ClientPageQuery, error) {
	var q ClientPageQuery
	if len(queries) > 1 {
		return q, ErrClientPage
	}
	if len(queries) == 1 {
		q = queries[0]
	}
	if q.Offset < 0 || q.Offset > 1<<31-1 || len(q.Search) > 64 {
		return q, ErrClientPage
	}
	q.Search = strings.ToLower(strings.TrimSpace(q.Search))
	return q, nil
}

// Select the bounded client page before evaluating package counts or joins.
const selectClientPage = `WITH page AS MATERIALIZED (
 SELECT ip,first_seen,last_seen FROM clients
 WHERE last_seen>=? AND (?='' OR instr(lower(ip),?)>0)
 ORDER BY last_seen DESC,ip LIMIT ? OFFSET ?
)`

func (s DB) ClientSummaries(ctx context.Context, queries ...ClientPageQuery) ([]ClientSummary, error) {
	return s.clientSummaries(ctx, queries, ClientPageSize)
}

func (s DB) ClientSummaryPage(ctx context.Context, q ClientPageQuery) (ClientSummaryPage, error) {
	rows, err := s.clientSummaries(ctx, []ClientPageQuery{q}, ClientPageSize+1)
	if err != nil {
		return ClientSummaryPage{}, err
	}
	return ClientSummaryPage{Clients: rows[:min(len(rows), ClientPageSize)], More: len(rows) > ClientPageSize}, nil
}

func (s DB) clientSummaries(ctx context.Context, queries []ClientPageQuery, limit int) ([]ClientSummary, error) {
	q, err := clientPageQuery(queries)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, selectClientPage+`
 SELECT c.ip,c.first_seen,c.last_seen,(
  SELECT count(*) FROM (
   SELECT name FROM package_requests WHERE ip=c.ip
   UNION SELECT name FROM client_report_packages WHERE ip=c.ip
  )
 ) FROM page c ORDER BY c.last_seen DESC,c.ip`, time.Now().Add(-ClientActivityRetention).Unix(), q.Search, q.Search, limit, q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClientSummary{}
	for rows.Next() {
		var client ClientSummary
		if err := rows.Scan(&client.IP, &client.FirstSeen, &client.LastSeen, &client.PackageCount); err != nil {
			return nil, err
		}
		out = append(out, client)
	}
	return out, rows.Err()
}

func (s DB) ClientPackages(ctx context.Context, ip string) (ClientPackageList, error) {
	out := ClientPackageList{PackageDetails: []ClientPackageDetail{}}
	rows, err := s.DB.QueryContext(ctx, `
WITH names AS (
 SELECT name FROM package_requests WHERE ip=?1
 UNION SELECT name FROM client_report_packages WHERE ip=?1
)
SELECT c.ip,c.first_seen,c.last_seen,coalesce(r.reported_at,0),n.name,
 coalesce(t.origin,p.origin,''),coalesce(q.version,''),coalesce(p.version,'')
FROM clients c
LEFT JOIN client_reports r ON r.ip=c.ip
LEFT JOIN names n ON true
LEFT JOIN package_requests q ON q.ip=c.ip AND q.name=n.name
LEFT JOIN tracked_packages t ON t.name=q.name
LEFT JOIN client_report_packages p ON p.ip=c.ip AND p.name=n.name
WHERE c.ip=?1 ORDER BY n.name`, ip)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ClientPackageDetail
		var name sql.NullString
		if err := rows.Scan(&out.IP, &out.FirstSeen, &out.LastSeen, &out.ReportedAt, &name, &p.Origin, &p.RequestedVersion, &p.ReportedVersion); err != nil {
			return out, err
		}
		if name.Valid {
			p.Name = name.String
			out.PackageDetails = append(out.PackageDetails, p)
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if out.IP == "" {
		return out, sql.ErrNoRows
	}
	out.PackageCount = len(out.PackageDetails)
	return out, nil
}

type ClientRecord struct {
	IP               string   `json:"ip"`
	FirstSeen        int64    `json:"first_seen"`
	LastSeen         int64    `json:"last_seen"`
	Packages         []string `json:"packages"`
	ReportedPackages []string `json:"reported_packages"`
	ReportedAt       int64    `json:"reported_at,omitempty"`
}

func (s DB) Clients(ctx context.Context, queries ...ClientPageQuery) ([]ClientRecord, error) {
	q, err := clientPageQuery(queries)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, selectClientPage+`
SELECT c.ip,c.first_seen,c.last_seen,coalesce(r.reported_at,0),p.name,p.version,coalesce(p.reported,0)
FROM page c
LEFT JOIN client_reports r ON c.ip=r.ip
LEFT JOIN (
 SELECT q.ip,q.name,q.version,0 AS reported FROM package_requests q JOIN page c ON c.ip=q.ip
 UNION ALL SELECT p.ip,p.name,p.version,1 FROM client_report_packages p JOIN page c ON c.ip=p.ip
) p ON c.ip=p.ip
ORDER BY c.last_seen DESC,c.ip,p.reported,p.name`, time.Now().Add(-ClientActivityRetention).Unix(), q.Search, q.Search, ClientPageSize, q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClientRecord{}
	for rows.Next() {
		var c ClientRecord
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

const clientsSchema = `CREATE TABLE IF NOT EXISTS clients (
 ip TEXT PRIMARY KEY, first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS clients_recent ON clients(last_seen DESC,ip);
`
