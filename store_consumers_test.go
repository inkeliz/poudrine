package poudrine

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClientPackageDetailsAndConsumers(t *testing.T) {
	repo, pkg := inventoryReportFixture(t)
	s, ctx := repo.store, t.Context()
	const first, second, unknown, quiet = "192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"
	for range 2 {
		if err := s.record(ctx, first, &pkg); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.record(ctx, quiet, nil); err != nil {
		t.Fatal(err)
	}
	installed := installedPackage{version: "2", origin: pkg.Origin, buildOrigin: pkg.BuildOrigin}
	if err := s.replaceReport(ctx, first, testReportProof(), map[string]installedPackage{
		pkg.Name: installed, "private-app": {origin: "local/app", version: "2.1-rc1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.replaceReport(ctx, second, testReportProof(), map[string]installedPackage{pkg.Name: installed}); err != nil {
		t.Fatal(err)
	}
	if err := s.replaceReport(ctx, unknown, testReportProof(), map[string]installedPackage{pkg.Name: {origin: "local/other", version: "1"}}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.clientSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected := []clientPackageDetail{
		{Name: "private-app", Origin: "local/app", ReportedVersion: "2.1-rc1"},
		{Name: pkg.Name, Origin: pkg.Origin, RequestedVersion: pkg.Version, ReportedVersion: "2"},
	}
	if len(rows) != 4 {
		t.Fatalf("client inventory %+v", rows)
	}
	counts := map[string]int{first: 2, second: 1, unknown: 1, quiet: 0}
	for _, client := range rows {
		if count, ok := counts[client.IP]; !ok || client.PackageCount != count || client.LastSeen == 0 {
			t.Fatalf("client summary %+v", client)
		}
	}
	client, err := s.clientPackages(ctx, first)
	if err != nil || client.PackageCount != 2 || !reflect.DeepEqual(client.PackageDetails, expected) {
		t.Fatalf("first client package details %+v, %v", client, err)
	}
	third, err := s.clientPackages(ctx, unknown)
	if err != nil || len(third.PackageDetails) != 1 || third.PackageDetails[0].Origin != "local/other" {
		t.Fatalf("unmatched package missing: %+v, %v", third, err)
	}
	packages, err := s.trackedPackages(ctx)
	if err != nil || len(packages) != 1 || packages[0].Consumers != 2 || packages[0].LastSeen < client.ReportedAt {
		t.Fatalf("package usage: %+v, %v", packages, err)
	}
	if err := s.replaceReport(ctx, second, testReportProof(), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.replaceReport(ctx, first, testReportProof(), nil); err != nil {
		t.Fatal(err)
	}
	packages, err = s.trackedPackages(ctx)
	if err != nil || len(packages) != 1 || packages[0].Consumers != 1 || !packages[0].Reported || !packages[0].Requested {
		t.Fatalf("request history after report replacement: %+v, %v", packages, err)
	}
}

func TestSignedClientSummaryAndPackages(t *testing.T) {
	dir := t.TempDir()
	db, err := openInventory(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := testAdmin(t).cfg
	cfg.DB, cfg.DataDir = db, dir
	a, err := NewAdmin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	priv := enrollAdmin(t, a)
	store := inventory{db}
	const ip = "fe80::1%em0"
	if err := store.record(t.Context(), ip, nil); err != nil {
		t.Fatal(err)
	}
	body := `{"ip":"` + ip + `"}`
	for _, target := range []string{clientSummaryPath, clientPackagesPath} {
		adminResponse(t, a, httptest.NewRequest(methodQuery, target, nil), 401)
	}
	query := func(target, body string, status int) string {
		return adminResponse(t, a, signedTestRequest(t, priv, methodQuery, target, []byte(body), time.Now()), status).Body.String()
	}
	query(clientPackagesPath, `{"ip":"invalid"}`, 400)
	query(clientPackagesPath, `{"ip":"127.0.0.1"}`, 404)
	var details clientPackageList
	if err := json.Unmarshal([]byte(query(clientPackagesPath, body, 200)), &details); err != nil || details.IP != ip || details.PackageCount != 0 || details.PackageDetails == nil {
		t.Fatalf("metadata-only client %+v, %v", details, err)
	}
	if err := store.replaceReport(t.Context(), ip, testReportProof(), map[string]installedPackage{"private-app": {origin: "local/app", version: "3"}}); err != nil {
		t.Fatal(err)
	}
	summary := query(clientSummaryPath, "{}", 200)
	if !strings.Contains(summary, `"package_count":1`) || strings.Contains(summary, "private-app") {
		t.Fatalf("unexpected client summary: %s", summary)
	}
	if !strings.Contains(query(clientPackagesPath, body, 200), `"reported_version":"3"`) {
		t.Fatal("reported version missing")
	}
}

func TestReportedLastSeenUpgrade(t *testing.T) {
	dir := t.TempDir()
	db, err := openInventory(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
ALTER TABLE reported_targets DROP COLUMN last_seen;
INSERT INTO clients VALUES('::1',1,9);
INSERT INTO tracked_packages VALUES('curl','ftp/curl','ftp/curl',1);
INSERT INTO reported_targets(name) VALUES('curl');
INSERT INTO client_reports VALUES('::1',9);
INSERT INTO client_report_packages VALUES('::1','curl','1','ftp/curl','ftp/curl');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	db, err = openInventory(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := inventory{db}
	if err := s.replaceReport(t.Context(), "::1", testReportProof(), nil); err != nil {
		t.Fatal(err)
	}
	packages, err := s.trackedPackages(t.Context())
	if err != nil || len(packages) != 1 || packages[0].Consumers != 0 || packages[0].LastSeen != 9 {
		t.Fatalf("last report timestamp after upgrade and removal: %+v, %v", packages, err)
	}
}
