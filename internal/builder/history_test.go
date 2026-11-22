package builder

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/testutil"
)

// Poudriere's result lists contain per-job duration in seconds rather than a
// finish timestamp. Two packages in a single invocation can span UTC buckets.
func TestPackageHistoryFromRetainedAndLiveResults(t *testing.T) {
	cfg := testutil.WorkerConfig(t.TempDir())
	now := time.Now().UTC().Truncate(time.Hour)
	started := now.Add(-3 * time.Hour)
	root := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports)
	run := filepath.Join(root, started.Format(BuildNameFormat))
	if err := os.MkdirAll(filepath.Join(run, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(run), filepath.Join(root, "latest")); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := *db
	history := PackageLogHistory{Cfg: cfg, Save: store.RecordBuilds}
	ctx := t.Context()
	build := BuildStatus{Active: true, Started: started}
	status := func() BuildStatus { return build }
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(run, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func(name string, at time.Time) {
		t.Helper()
		origin := map[string]string{"curl-1": "ftp/curl", "git-2": "devel/git", "fetch-1": "devel/fetch", "broken-1": "local/broken", "empty-1": "local/empty", "legacy-1": "local/legacy"}[name]
		write("logs/"+name+".log", "build of "+origin+" | "+name+" ended at "+at.Format(time.UnixDate)+"\n")
		path := filepath.Join(run, "logs", name+".log")
		// A restored log may have a different mtime; the dated footer takes precedence.
		if err := os.Chtimes(path, now, now); err != nil {
			t.Fatal(err)
		}
	}
	stamp("curl-1", now.Add(-time.Hour))
	// A previous Poudrine release already imported this success without its
	// duration. Replaying the retained log must fill it without a second event.
	if _, err := db.Exec(`INSERT INTO package_builds VALUES(?, 'curl-1', 'succeeded', ?)`,
		started.UnixNano(), now.Add(-time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO push_build_watch VALUES('already-notified',0)"); err != nil {
		t.Fatal(err)
	}
	// Git finishes after Poudriere has appended its result, while the bulk is still active.
	stamp("fetch-1", now)
	write(".poudriere.ports.built", "ftp/curl curl-1 9999\n")
	write(".poudriere.ports.fetched", "devel/fetch fetch-1 10\n")
	write(".poudriere.ports.skipped", "devel/git git-ignored ignored\n")
	scan := func() {
		t.Helper()
		if err := history.Scan(ctx, status); err != nil {
			t.Fatal(err)
		}
	}
	counts := func(wantBuilt, wantFailed int) {
		t.Helper()
		h, err := store.BuildHistory(ctx, "hour", now.Add(time.Second))
		if err != nil || h.Totals.Succeeded != wantBuilt || h.Totals.Failed != wantFailed || h.Buckets[len(h.Buckets)-2].Succeeded != 1 {
			t.Fatalf("package history %+v: %v", h, err)
		}
	}
	scan()
	counts(1, 0)
	write(".poudriere.ports.built", "ftp/curl curl-1 9999\ndevel/git git-2 1")
	scan()
	counts(1, 0)
	write(".poudriere.ports.built", "ftp/curl curl-1 9999\ndevel/git git-2 1\n")
	scan()
	counts(1, 0)
	write("logs/git-2.log", "compile output, cleanup pending\n")
	scan()
	counts(1, 0)
	stamp("git-2", now)
	scan()
	counts(2, 0)
	stamp("broken-1", now)
	stamp("empty-1", now)
	stamp("legacy-1", now)
	write(".poudriere.ports.failed", "local/broken broken-1 compile unknown 30\nlocal/empty empty-1 build  17\nlocal/legacy legacy-1 build 404\n")
	build.Active = false
	scan()
	counts(2, 3)
	recent, err := store.BuildHistory(ctx, "hour", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	wantSeconds := map[string]int64{"curl-1": 9999, "git-2": 1, "broken-1": 30, "empty-1": 17}
	for _, r := range recent.Recent {
		want, known := wantSeconds[r.Package]
		if (r.BuildSeconds != nil) != known || known && *r.BuildSeconds != want {
			t.Fatalf("wrong duration for %s: %v", r.Package, r.BuildSeconds)
		}
	}
	var pushed int
	if err := db.QueryRow("SELECT count(*) FROM push_build_queue WHERE package='curl-1'").Scan(&pushed); err != nil || pushed != 0 {
		t.Fatalf("backfill queued %d duplicate alerts: %v", pushed, err)
	}
	// Restart with the old run in the log tree. Reimporting recorded results
	// counts them once. Removing the original logs preserves their recorded outcomes.
	history = PackageLogHistory{Cfg: cfg, Save: store.RecordBuilds}
	scan()
	counts(2, 3)
	if err := os.RemoveAll(run); err != nil {
		t.Fatal(err)
	}
	scan()
	counts(2, 3)
}

func TestPackageHistoryRetriesFailedWrites(t *testing.T) {
	cfg := testutil.WorkerConfig(t.TempDir())
	start := time.Now().UTC().Add(-time.Minute)
	run := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports, start.Format(BuildNameFormat))
	if err := os.MkdirAll(filepath.Join(run, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "logs", "curl-1.log"), []byte("complete"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, ".poudriere.ports.built"), []byte("ftp/curl curl-1 9\n"), 0644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := PackageLogHistory{Cfg: cfg, Save: func(ctx context.Context, records []database.BuildRecord) error {
		calls++
		if len(records) != 1 || records[0].Package != "curl-1" {
			t.Fatal(records)
		}
		if calls == 1 {
			return os.ErrPermission
		}
		return nil
	}}
	for range 3 {
		if err := h.Scan(t.Context(), func() BuildStatus { return BuildStatus{} }); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("database write attempts %d", calls)
	}
}
