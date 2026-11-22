package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestPackageInventoryWithSupervisedOrigins(t *testing.T) {
	a := testAdmin(t)
	key := enrollAdmin(t, a)
	cfg := testutil.WorkerConfig(t.TempDir())
	if err := os.WriteFile(cfg.PackagesFile, []byte("www/nginx\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := builder.NewBuilder(cfg, func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := b.Checkpoint()
	state.Status.PoudriereBytes = new(int64(8192))
	b.RestoreCheckpoint(state)
	a.cfg.Packages = func(_ context.Context, origins []string) (catalog.PackageInventoryView, error) {
		return catalog.ManagementPackages(nil, nil, origins, nil, nil, nil), nil
	}
	a, _ = supervisedTestAdmin(t, a.cfg, key, b)
	adminResponse(t, a, httptest.NewRequest(routes.Packages.Method, routes.Packages.Path, nil), 401)
	req := testutil.SignedRequest(t, key, routes.Packages.Method, routes.Packages.Path, []byte("{}"), time.Now())
	var result catalog.PackageInventoryView
	if err := json.Unmarshal(adminResponse(t, a, req, 200).Body.Bytes(), &result); err != nil || !result.OriginsReady || result.TrackedOrigins != 1 || len(result.Packages) != 1 || result.Packages[0].Origin != "www/nginx" {
		t.Fatalf("signed origin lookup: %+v (%v)", result, err)
	}
	if result.PoudriereBytes == nil || *result.PoudriereBytes != 8192 {
		t.Fatalf("missing worker disk usage: %+v", result)
	}
}

func TestSnapshotAvailability(t *testing.T) {
	a := testAdmin(t)
	key := enrollAdmin(t, a)
	cfg := testutil.WorkerConfig(t.TempDir())
	if err := os.WriteFile(cfg.PackagesFile, []byte("www/nginx\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.cfg.ActiveSettings.PackagesFile = cfg.PackagesFile
	a.cfg.Build = func() (builder.BuildStatus, error) { return builder.BuildStatus{}, os.ErrNotExist }
	query := func(route routes.Route) *http.Request {
		return testutil.SignedRequest(t, key, route.Method, route.Path, []byte("{}"), time.Now())
	}
	adminResponse(t, a, query(routes.BuildStatus), http.StatusServiceUnavailable)
	a.cfg.Packages = func(_ context.Context, origins []string) (catalog.PackageInventoryView, error) {
		return catalog.ManagementPackages(nil, nil, origins, nil, nil, nil), nil
	}
	w := adminResponse(t, a, query(routes.Packages), http.StatusOK)
	var view catalog.PackageInventoryView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || view.TrackedOrigins != 1 || view.PoudriereBytes != nil {
		t.Fatalf("inventory with unavailable worker status: %+v, %v", view, err)
	}
	// A partial inventory cannot treat the supervisor's signature rejection
	// as a worker outage.
	supervised, _ := supervisedTestAdmin(t, a.cfg, key, nil)
	unsigned := httptest.NewRequest(routes.Packages.Method, routes.Packages.Path, nil)
	reply, err := supervised.workerSnapshot(unsigned, []byte("{}"), allowPartialInventory)
	if err == nil || reply.Status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated worker snapshot: %+v, %v", reply, err)
	}
}
