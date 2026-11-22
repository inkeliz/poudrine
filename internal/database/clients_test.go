package database

import (
	"testing"

	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestReportedLastSeenUpgrade(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, false)
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
	db, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := *db
	if err := s.ReplaceReport(t.Context(), "::1", testutil.ReportProof(), nil); err != nil {
		t.Fatal(err)
	}
	packages, err := s.TrackedPackages(t.Context())
	if err != nil || len(packages) != 1 || packages[0].Consumers != 0 || packages[0].LastSeen != 9 {
		t.Fatalf("last report timestamp after upgrade and removal: %+v, %v", packages, err)
	}
}
