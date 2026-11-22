package public

import (
	"reflect"
	"testing"

	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestClientPackageDetailsAndConsumers(t *testing.T) {
	repo, pkg := inventoryReportFixture(t)
	s, ctx := repo.Store, t.Context()
	const first, second, unknown, quiet = "192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"
	for range 2 {
		if err := s.Record(ctx, first, &pkg); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Record(ctx, quiet, nil); err != nil {
		t.Fatal(err)
	}
	installed := database.InstalledPackage{Version: "2", Origin: pkg.Origin, BuildOrigin: pkg.BuildOrigin}
	if err := s.ReplaceReport(ctx, first, testutil.ReportProof(), map[string]database.InstalledPackage{
		pkg.Name: installed, "private-app": {Origin: "local/app", Version: "2.1-rc1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceReport(ctx, second, testutil.ReportProof(), map[string]database.InstalledPackage{pkg.Name: installed}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceReport(ctx, unknown, testutil.ReportProof(), map[string]database.InstalledPackage{pkg.Name: {Origin: "local/other", Version: "1"}}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ClientSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected := []database.ClientPackageDetail{
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
	client, err := s.ClientPackages(ctx, first)
	if err != nil || client.PackageCount != 2 || !reflect.DeepEqual(client.PackageDetails, expected) {
		t.Fatalf("first client package details %+v, %v", client, err)
	}
	third, err := s.ClientPackages(ctx, unknown)
	if err != nil || len(third.PackageDetails) != 1 || third.PackageDetails[0].Origin != "local/other" {
		t.Fatalf("unmatched package missing: %+v, %v", third, err)
	}
	packages, err := s.TrackedPackages(ctx)
	if err != nil || len(packages) != 1 || packages[0].Consumers != 2 || packages[0].LastSeen < client.ReportedAt {
		t.Fatalf("package usage: %+v, %v", packages, err)
	}
	if err := s.ReplaceReport(ctx, second, testutil.ReportProof(), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceReport(ctx, first, testutil.ReportProof(), nil); err != nil {
		t.Fatal(err)
	}
	packages, err = s.TrackedPackages(ctx)
	if err != nil || len(packages) != 1 || packages[0].Consumers != 1 || !packages[0].Reported || !packages[0].Requested {
		t.Fatalf("request history after report replacement: %+v, %v", packages, err)
	}
}
