package admin

import (
	"context"
	"net/http"
	"net/netip"

	"github.com/inkeliz/poudrine/internal/database"
)

func (a *Admin) clientSummaries(ctx context.Context, q database.ClientPageQuery) (database.ClientSummaryPage, error) {
	return a.cfg.DB.ClientSummaryPage(ctx, q)
}

func (a *Admin) clientRecords(ctx context.Context, q database.ClientPageQuery) ([]database.ClientRecord, error) {
	return a.cfg.Clients(ctx, q)
}

type clientQuery struct {
	IP string `json:"ip"`
}

func (a *Admin) clientPackages(ctx context.Context, args clientQuery) (database.ClientPackageList, error) {
	if _, err := netip.ParseAddr(args.IP); err != nil {
		return database.ClientPackageList{}, invalidQuery("Expected a client IP address")
	}
	return a.cfg.DB.ClientPackages(ctx, args.IP)
}

func (a *Admin) packageInventory(w http.ResponseWriter, r *http.Request, body []byte) {
	if a.cfg.Packages == nil {
		http.Error(w, "Package inventory unavailable", http.StatusServiceUnavailable)
		return
	}
	// The cache and client activity remain available when the worker is offline.
	snapshot, err := a.workerSnapshot(r, body, allowPartialInventory)
	if err != nil {
		writeBuildError(w, snapshot.Status, err)
		return
	}
	view, err := a.cfg.Packages(r.Context(), snapshot.Origins)
	if err != nil {
		adminInternal(w, err)
		return
	}
	view.PoudriereBytes = snapshot.PoudriereBytes
	adminJSON(w, view)
}
