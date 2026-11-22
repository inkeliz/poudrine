package database

import (
	"testing"
	"time"
)

func TestRecentInventoryBuilds(t *testing.T) {
	db, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := *db
	now := time.Now().UTC().Truncate(time.Second)
	const abi = "freebsd:15:amd64"
	const otherABI = "freebsd:15:aarch64"
	build := func(pkg, origin, abi string, offset int) BuildRecord {
		start := now.Add(time.Duration(offset) * time.Minute)
		return BuildRecord{Package: pkg, Origin: origin, ABI: abi, Started: start, Finished: start.Add(time.Second), Outcome: "succeeded"}
	}
	firefox := build("firefox-161.0,2", "www/firefox", abi, -5)
	firefox.BuildSeconds = new(int64(800))
	bare := firefox
	bare.Origin, bare.ABI, bare.BuildSeconds = "", "", nil // replayed metadata for the same run
	olderVersion := build("firefox-160.1,2", "www/firefox", abi, -10)
	fail := build("firefox-162.0,2", "www/firefox", abi, -1)
	fail.Outcome = "failed"
	arm := build("lib-example-1", "devel/lib-example@default", otherABI, -50)
	amd := build("lib-example-2", "devel/lib-example@default", abi, -60)
	armReplay := arm
	armReplay.ABI = "" // replayed before its jail ABI could be read
	earlierStart := build("lib-example-0", amd.Origin, amd.ABI, -70)
	earlierStart.Finished = amd.Finished.Add(time.Second)
	otherPort := build("lib-example-3", "devel/lib-example@experimental", abi, -80)
	expired := build("removed-1", "devel/removed", "", -1)
	expired.Started, expired.Finished = now.AddDate(-2, 0, 0), now.AddDate(-2, 0, 1)
	if err := s.RecordBuilds(t.Context(), []BuildRecord{olderVersion, bare, firefox, fail, armReplay, arm, amd, earlierStart, otherPort, expired}); err != nil {
		t.Fatal(err)
	}
	// Older installations have only package_builds (and sometimes job timings).
	// A newer import can still use unambiguous metadata from the same package.
	rebuild := build(firefox.Package, "", "", -2)
	legacy := build("legacy-tool-3.0_2", "", "", -3)
	for _, r := range []BuildRecord{legacy, rebuild} {
		_, err = db.Exec("INSERT INTO package_builds VALUES (?,?,'succeeded',?)", r.Started.UnixNano(), r.Package, r.Finished.UnixNano())
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO package_build_times VALUES (?,?,'succeeded',?)", rebuild.Started.UnixNano(), rebuild.Package, 123); err != nil {
		t.Fatal(err)
	}
	builds, err := s.RecentInventoryBuilds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != 5 {
		t.Fatalf("incorrect number of distinct successful builds: %+v", builds)
	}
	found := make(map[string]BuildRecord)
	for _, r := range builds {
		found[r.Package] = r
	}
	f := found[firefox.Package]
	if !f.Finished.Equal(rebuild.Finished) || !f.Started.Equal(rebuild.Started) || f.Origin != firefox.Origin || f.ABI != abi || f.BuildSeconds == nil || *f.BuildSeconds != 123 {
		t.Fatalf("latest Firefox build lost its identity or used another run's timing: %+v", f)
	}
	for _, want := range []BuildRecord{legacy, arm, earlierStart, otherPort} {
		got := found[want.Package]
		if got != want {
			t.Fatalf("got %+v for %+v", got, want)
		}
	}
}
