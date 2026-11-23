package admin

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestSignedClientSummaryAndPackages(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := testAdmin(t).cfg
	cfg.DB, cfg.DataDir = db, dir
	a, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	priv := enrollAdmin(t, a)
	store := *db
	const ip = "fe80::1%em0"
	if err := store.Record(t.Context(), ip, nil); err != nil {
		t.Fatal(err)
	}
	body := `{"ip":"` + ip + `"}`
	for _, target := range []string{routes.ClientSummary.Path, routes.ClientPackages.Path} {
		adminResponse(t, a, httptest.NewRequest(broker.MethodQuery, target, nil), 401)
	}
	query := func(target, body string, status int) string {
		return adminResponse(t, a, testutil.SignedRequest(t, priv, broker.MethodQuery, target, []byte(body), time.Now()), status).Body.String()
	}
	query(routes.ClientPackages.Path, `{"ip":"invalid"}`, 400)
	query(routes.ClientPackages.Path, `{"ip":"127.0.0.1"}`, 404)
	var details database.ClientPackageList
	if err := json.Unmarshal([]byte(query(routes.ClientPackages.Path, body, 200)), &details); err != nil || details.IP != ip || details.PackageCount != 0 || details.PackageDetails == nil {
		t.Fatalf("metadata-only client %+v, %v", details, err)
	}
	if err := store.ReplaceReport(t.Context(), ip, testutil.ReportProof(), map[string]database.InstalledPackage{"private-app": {Origin: "local/app", Version: "3"}}); err != nil {
		t.Fatal(err)
	}
	summary := query(routes.ClientSummary.Path, "{}", 200)
	if !strings.Contains(summary, `"package_count":1`) || strings.Contains(summary, "private-app") {
		t.Fatalf("unexpected client summary: %s", summary)
	}
	if !strings.Contains(query(routes.ClientPackages.Path, body, 200), `"reported_version":"3"`) {
		t.Fatal("reported version missing")
	}
}
