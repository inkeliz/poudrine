package database

import (
	"maps"
	"testing"
	"time"
)

func TestLastBuildDurations(t *testing.T) {
	db, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	now := time.Now().UTC()
	const origin = "devel/tool-kit@default"
	const abi = "freebsd:15:amd64"
	build := func(minute int, pkg, origin, abi string, seconds int64) BuildRecord {
		started := now.Add(time.Duration(minute) * time.Minute)
		r := BuildRecord{Started: started, Finished: started.Add(time.Second), Outcome: "succeeded", Package: pkg, Origin: origin, ABI: abi}
		if seconds >= 0 {
			r.BuildSeconds = &seconds
		}
		return r
	}
	previous := build(-8, "tool-kit-1.0_1,2", origin, abi, 120)
	timed := build(-7, "tool-kit-2", origin, abi, 180)
	untimed := build(-4, timed.Package, origin, abi, -1)
	current := build(0, "tool-kit-3", origin, abi, 300)
	failed := build(-1, "tool-kit-2_1", origin, abi, 5)
	failed.Outcome = "failed"
	arm := build(-2, "tool-kit-2", origin, "freebsd:15:aarch64", 210)
	flavor := build(-2, "tool-kit-1", "devel/tool-kit@alt", abi, 90)
	if err := db.RecordBuilds(ctx, []BuildRecord{
		previous, timed, untimed, current, failed, arm, flavor,
		build(-1, "tool-kit-extras-2", origin, abi, 400),
		build(-1, "tool-kit-2.2", origin, abi, 0),
		build(-1, "tool-kit-2.3", origin, "", 1),
		build(-1, "tool-kit-2.4", "", abi, 1),
		build(-3, "replay-1", "devel/replay", abi, 0),
		build(-6, "replay-1", "devel/replay", abi, 30),
	}); err != nil {
		t.Fatal(err)
	}
	local := BuildTimeTarget{current.Package, origin, "FreeBSD:15:x86:64"}
	armTarget := BuildTimeTarget{current.Package, origin, arm.ABI}
	flavorTarget := BuildTimeTarget{current.Package, flavor.Origin, abi}
	replayed := BuildTimeTarget{"replay-2", "devel/replay", abi}
	targets := []BuildTimeTarget{local, armTarget, flavorTarget, replayed,
		{"unknown-1", origin, abi}, {current.Package, origin, ""}, {current.Package, "", abi}, {current.Package, origin, "freebsd:14:amd64"},
	}
	want := map[BuildTimeTarget]int64{local: 180, armTarget: 210, flavorTarget: 90, replayed: 30}
	got, err := db.LastBuildDurations(ctx, current.Started, targets)
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("durations before current run = %v, %v; want %v", got, err, want)
	}
	// An untimed rebuild preserves the previous estimate without changing the
	// inventory's duration for the latest build of the same exact version.
	inventory, err := db.CompiledPackageTimes(ctx, []string{timed.Package})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range inventory {
		if item.ABI == abi && item.Seconds != nil {
			t.Fatalf("latest inventory duration = %d; want unknown", *item.Seconds)
		}
	}
	want[local] = 300
	got, err = db.LastBuildDurations(ctx, time.Time{}, targets)
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("durations across all runs = %v, %v; want %v", got, err, want)
	}
}

func TestLastBuildDurationsFromExistingInstall(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	target := BuildTimeTarget{"test-2", "devel/test", "freebsd:15:amd64"}
	// Older installations can have a published timing even after the
	// per-run history and logs have expired.
	_, err = db.Exec(`DROP TABLE previous_package_build_times;
INSERT INTO compiled_package_times(package,origin,abi,started_ns,seconds) VALUES ('test-2',?,?,?,75)`,
		target.Origin, target.ABI, start.Add(-time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	current := BuildRecord{Package: target.Package, Origin: target.Origin, ABI: target.ABI, Started: start, Finished: start.Add(time.Second), Outcome: "succeeded", BuildSeconds: new(int64(45))}
	if err := db.RecordBuilds(t.Context(), []BuildRecord{current}); err != nil {
		t.Fatal(err)
	}
	for cutoff, want := range map[time.Time]int64{start: 75, start.Add(time.Second): 45} {
		got, err := db.LastBuildDurations(t.Context(), cutoff, []BuildTimeTarget{target})
		if err != nil || got[target] != want {
			t.Fatalf("duration before %v = %v, %v; want %d", cutoff, got, err, want)
		}
	}
}
