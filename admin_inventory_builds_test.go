package poudrine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInventoryFindsNativeBuildBeforeRepositoryPublication(t *testing.T) {
	cfg := workerTestConfig(t.TempDir())
	day := time.Now().UTC().AddDate(0, 0, -1)
	finished := time.Date(day.Year(), day.Month(), day.Day(), 14, 43, 12, 0, time.UTC)
	started := finished.Add(-time.Hour)
	run := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports, started.Format(buildNameFormat))
	if err := os.MkdirAll(filepath.Join(run, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	const firefox = "firefox-161.0,2"
	for path, content := range map[string]string{
		".poudriere.ports.built":   "www/firefox " + firefox + " 1827\n",
		".poudriere.ports.failed":  "www/broken broken-1 build compiler_error 2\n",
		".poudriere.ports.fetched": "www/fetched fetched-1\n",
		"logs/" + firefox + ".log": "building for: FreeBSD jail 15.0-RELEASE FreeBSD 15.0-RELEASE amd64\n" +
			"build of www/firefox | " + firefox + " ended at " + finished.Format(time.UnixDate) + "\n",
		"logs/broken-1.log":  "build of www/broken | broken-1 ended at " + finished.Format(time.UnixDate) + "\n",
		"logs/fetched-1.log": "Fetched from FreeBSD\nbuild of www/fetched | fetched-1 ended at " + finished.Format(time.UnixDate) + "\n",
	} {
		if err := os.WriteFile(filepath.Join(run, path), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openInventory(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := inventory{db}
	logs := packageLogHistory{cfg: cfg, save: store.recordBuilds}
	if err := logs.scan(t.Context(), func() BuildStatus { return BuildStatus{Started: started, Active: true} }); err != nil {
		t.Fatal(err)
	}
	builds, err := store.recentInventoryBuilds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	view := managementPackages(nil, nil, nil, nil, nil, builds)
	if len(view.Packages) != 1 || view.CatalogReady || view.TrackedOrigins != 0 || view.CachedCount != 0 || view.CompiledBytes != nil {
		t.Fatalf("unexpected inventory while bulk is still running: %+v", view)
	}
	row := view.Packages[0]
	if row.Name != "firefox" || row.Version != "161.0,2" || row.BuildOrigin != "www/firefox" || !row.HistoryOnly ||
		row.Tracked || row.Cached || row.Size != nil || row.BuildSeconds == nil || *row.BuildSeconds != 1827 ||
		row.LastSeen != finished.Unix() || row.Consumers == nil || *row.Consumers != 0 {
		t.Fatalf("Firefox result before publication: %+v", row)
	}
}

func TestInventoryReadsExistingSuccessWithoutNativeLogs(t *testing.T) {
	dir := t.TempDir()
	db, err := openInventory(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// Earlier releases can have a completed package event without origin,
	// ABI or duration. Reopening the inventory must not require its log.
	finished := time.Now().UTC().Add(-time.Minute)
	started := finished.Add(-time.Hour)
	_, err = db.Exec(`INSERT INTO package_builds(started_ns,package,outcome,finished_ns)
		VALUES (?, 'firefox-esr-140.5_1,2', 'succeeded', ?)`, started.UnixNano(), finished.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openInventory(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	builds, err := (inventory{db}).recentInventoryBuilds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	view := managementPackages(nil, nil, nil, nil, nil, builds)
	if len(view.Packages) != 1 {
		t.Fatalf("existing successful build: %+v", view)
	}
	row := view.Packages[0]
	if !row.HistoryOnly || row.Name != "firefox-esr" || row.Version != "140.5_1,2" || row.BuildSeconds != nil || row.LastSeen != finished.Unix() {
		t.Fatalf("imported legacy Firefox build: %+v", row)
	}
}

func TestInventoryReconcilesBuildWithPublishedArchives(t *testing.T) {
	const abi = "freebsd:15:amd64"
	const origin = "www/firefox"
	finished := time.Now().UTC().Add(-time.Minute)
	build := BuildRecord{
		Package: "firefox-161.0,2", Origin: origin, ABI: abi, Started: finished.Add(-time.Hour),
		Finished: finished, Outcome: "succeeded", BuildSeconds: new(int64(1827)),
	}
	cached := CatalogPackage{Name: "firefox", Origin: origin, BuildOrigin: origin, ABI: abi, Version: "159.0,2", Size: 11}
	for _, tc := range []struct {
		name, published, origin, abi string
		wantHistory                  bool
	}{
		{"older published version", "160.0,2", origin, abi, true},
		{"matching published archive", "161.0,2", origin, abi, false},
		{"another build origin", "161.0,2", "www/other", abi, true},
		{"another jail ABI", "161.0,2", origin, "freebsd:14:amd64", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := CatalogPackage{
				Name: "firefox", Version: tc.published, Origin: tc.origin, BuildOrigin: tc.origin, ABI: tc.abi,
				Local: true, Path: "All/firefox-" + tc.published + ".pkg", Size: 37,
			}
			snapshot := &catalogSnapshot{
				byPath: map[string]CatalogPackage{local.Path: local}, pathByName: map[string]string{"firefox": local.Path},
				compiled: map[string]CatalogPackage{"firefox": local}, compiledAt: map[string]time.Time{"firefox": finished.Add(-24 * time.Hour)},
				displaced: map[string]CatalogPackage{"firefox": cached},
			}
			cache := []cachedArchive{{Key: "cached", Package: cached, Used: finished.Add(-2 * time.Hour)}}
			view := managementPackages(snapshot, nil, nil, cache, nil, []BuildRecord{build})
			if view.TrackedOrigins != 0 || view.CachedCount != 1 || view.CachedBytes != cached.Size ||
				view.CompiledBytes == nil || *view.CompiledBytes != local.Size {
				t.Fatalf("inventory counts changed: %+v", view)
			}
			var historical, published, downloaded int
			for _, row := range view.Packages {
				switch {
				case row.HistoryOnly:
					historical++
					if row.Version != "161.0,2" || row.BuildOrigin != origin || row.Size != nil || row.LastSeen != finished.Unix() {
						t.Fatalf("unexpected recorded build: %+v", row)
					}
				case row.Cached && row.Version == cached.Version:
					downloaded++
				case row.Compiled && row.Version == tc.published:
					published++
					if !tc.wantHistory && (row.LastSeen != finished.Unix() || row.BuildSeconds == nil || *row.BuildSeconds != 1827) {
						t.Fatalf("matching archive lost its build time: %+v", row)
					}
				default:
					t.Fatalf("unexpected inventory row: %+v", row)
				}
			}
			if (historical == 1) != tc.wantHistory || historical > 1 || published != 1 || downloaded != 1 {
				t.Fatalf("expected one published, one cached, history=%t: %+v", tc.wantHistory, view.Packages)
			}
		})
	}
}
