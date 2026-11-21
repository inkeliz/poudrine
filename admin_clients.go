package poudrine

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/netip"
)

const clientSummaryPath = "/api/admin/clients/summary"
const clientPackagesPath = "/api/admin/clients/packages"

type clientSummary struct {
	IP           string `json:"ip"`
	FirstSeen    int64  `json:"first_seen"`
	LastSeen     int64  `json:"last_seen"`
	PackageCount int    `json:"package_count"`
}

type clientPackageDetail struct {
	Name             string `json:"name"`
	Origin           string `json:"origin"`
	RequestedVersion string `json:"requested_version,omitempty"`
	ReportedVersion  string `json:"reported_version,omitempty"`
}

type clientPackageList struct {
	clientSummary
	ReportedAt     int64                 `json:"reported_at,omitempty"`
	PackageDetails []clientPackageDetail `json:"package_details"`
}

func (s inventory) clientSummaries(ctx context.Context) ([]clientSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.ip,c.first_seen,c.last_seen,(
 SELECT count(*) FROM (
   SELECT name FROM package_requests WHERE ip=c.ip
   UNION SELECT name FROM client_report_packages WHERE ip=c.ip
 )
)
FROM clients c ORDER BY c.last_seen DESC,c.ip`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []clientSummary{}
	for rows.Next() {
		var client clientSummary
		if err := rows.Scan(&client.IP, &client.FirstSeen, &client.LastSeen, &client.PackageCount); err != nil {
			return nil, err
		}
		out = append(out, client)
	}
	return out, rows.Err()
}

func (s inventory) clientPackages(ctx context.Context, ip string) (clientPackageList, error) {
	out := clientPackageList{PackageDetails: []clientPackageDetail{}}
	rows, err := s.db.QueryContext(ctx, `
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
		var p clientPackageDetail
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

func (a *Admin) clientSummaries(w http.ResponseWriter, r *http.Request, _ []byte) {
	clients, err := (inventory{a.cfg.DB}).clientSummaries(r.Context())
	if err != nil {
		adminInternal(w, err)
		return
	}
	adminJSON(w, clients)
}

func (a *Admin) clientPackages(w http.ResponseWriter, r *http.Request, body []byte) {
	var args struct {
		IP string `json:"ip"`
	}
	if !queryArgs(w, body, &args) {
		return
	}
	ip := args.IP
	if _, err := netip.ParseAddr(ip); err != nil {
		http.Error(w, "Expected a client IP address", http.StatusBadRequest)
		return
	}
	client, err := (inventory{a.cfg.DB}).clientPackages(r.Context(), ip)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		adminInternal(w, err)
		return
	}
	adminJSON(w, client)
}
