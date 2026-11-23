package database

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestClientRetentionPreservesPinnedIdentity(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ctx := t.Context()
	oldProof := testutil.ReportProof()
	pkg := testutil.Package("curl", "body")
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		if err := db.Record(ctx, ip, &pkg); err != nil {
			t.Fatal(err)
		}
		proof := testutil.ReportProof()
		if ip == "192.0.2.1" {
			proof = oldProof
		}
		if err := db.ReplaceReport(ctx, ip, proof, map[string]InstalledPackage{pkg.Name: {Version: pkg.Version, Origin: pkg.Origin}}); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-ClientActivityRetention - time.Second).Unix()
	for _, table := range []string{"clients", "package_requests", "client_reports"} {
		column := "last_seen"
		if table == "client_reports" {
			column = "reported_at"
		}
		if _, err := db.Exec("UPDATE "+table+" SET "+column+"=? WHERE ip='192.0.2.1'", old); err != nil {
			t.Fatal(err)
		}
	}
	// Restore the legacy FK to exercise the migration on an existing install.
	if _, err := db.Exec(`ALTER TABLE client_report_keys RENAME TO old_keys;
 CREATE TABLE client_report_keys(ip TEXT PRIMARY KEY REFERENCES clients(ip),public_key BLOB NOT NULL CHECK(length(public_key)=32));
 INSERT INTO client_report_keys SELECT * FROM old_keys;
 DROP TABLE old_keys;`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CleanupClientActivity(ctx, now); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"clients", "package_requests", "client_reports", "client_report_packages"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s: count %d, %v", table, count, err)
		}
	}
	var pins int
	if err := db.QueryRow("SELECT count(*) FROM client_report_keys").Scan(&pins); err != nil || pins != 2 {
		t.Fatalf("lost identity pins: %d, %v", pins, err)
	}
	other := testutil.ReportProof()
	other.Key, _, err = ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceReport(ctx, "192.0.2.1", other, nil); !errors.Is(err, ErrReportKey) {
		t.Fatalf("expired client could change identity: %v", err)
	}
	proof := testutil.ReportProof()
	proof.Key = oldProof.Key
	if err := db.ReplaceReport(ctx, "192.0.2.1", proof, nil); err != nil {
		t.Fatalf("original identity could not return: %v", err)
	}
}

func TestClientPagesAreBoundedBeforePackageJoins(t *testing.T) {
	db, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Unix()
	for _, name := range []string{"curl", "wget"} {
		if _, err := db.Exec(trackPackageSQL, name, "www/"+name, "www/"+name, now); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2*ClientPageSize+3; i++ {
		ip := fmt.Sprintf("192.0.2.%d", i)
		if _, err := db.Exec("INSERT INTO clients VALUES(?,?,?)", ip, now, now); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"curl", "wget"} {
			if _, err := db.Exec("INSERT INTO package_requests VALUES(?,?,?,?)", ip, name, "1", now); err != nil {
				t.Fatal(err)
			}
		}
	}
	seen := map[string]bool{}
	for offset := 0; ; offset += ClientPageSize {
		page, err := db.ClientSummaryPage(t.Context(), ClientPageQuery{Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Clients) > ClientPageSize {
			t.Fatal("unbounded client page")
		}
		records, err := db.Clients(t.Context(), ClientPageQuery{Offset: offset})
		if err != nil || len(records) != len(page.Clients) {
			t.Fatalf("record page: %d, %v", len(records), err)
		}
		for i, c := range page.Clients {
			if c.PackageCount != 2 || records[i].IP != c.IP || len(records[i].Packages) != 2 {
				t.Fatalf("client packages truncated or miscounted: %+v, %+v", c, records[i])
			}
			if seen[c.IP] {
				t.Fatal("duplicate across stable pages")
			}
			seen[c.IP] = true
		}
		if !page.More {
			break
		}
	}
	if len(seen) != 2*ClientPageSize+3 {
		t.Fatalf("missing clients: %d", len(seen))
	}
	page, err := db.ClientSummaryPage(t.Context(), ClientPageQuery{Search: ".52"})
	if err != nil || len(page.Clients) != 1 {
		t.Fatalf("search did not reach later pages: %+v, %v", page, err)
	}
	if _, err := db.Clients(t.Context(), ClientPageQuery{Offset: -1}); !errors.Is(err, ErrClientPage) {
		t.Fatal("negative offset accepted")
	}
}
