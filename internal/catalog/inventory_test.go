package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestInventoryExplainsUnpublishedVersion(t *testing.T) {
	const abi = "freebsd:15:amd64"
	record := database.BuildRecord{Package: "firefox-161.0,2", Outcome: "succeeded", Origin: "www/firefox", ABI: abi}
	upstream := packageinfo.Package{Name: "firefox", Origin: record.Origin, BuildOrigin: record.Origin, ABI: abi,
		Version: "160.0,2", Path: "All/firefox-160.0,2.pkg"}
	s := &Snapshot{ByPath: map[string]packageinfo.Package{upstream.Path: upstream}, PathByName: map[string]string{"firefox": upstream.Path}}
	cat := &Catalog{}
	check := func(want string) {
		t.Helper()
		view := ManagementPackages(s, nil, nil, nil, nil, []database.BuildRecord{record})
		DescribeUnpublished(view.Packages, s, cat.LiveScan.Load())
		if len(view.Packages) != 1 || !view.Packages[0].HistoryOnly || !strings.Contains(view.Packages[0].PublicationNote, want) {
			t.Fatalf("want unpublished reason %q, got %+v", want, view.Packages)
		}
	}
	check("pkg currently offers 160.0,2")
	s.LiveHolds = map[string]liveHold{record.Package: {pkg: packageinfo.Package{Origin: record.Origin, ABI: abi}, Reason: "Waiting for nss"}}
	check("Waiting for nss")
	record.ABI = "freebsd:14:amd64"
	check("No matching compiled archive")
	record.ABI = abi
	s.LiveHolds = nil
	cat.recordLiveScan(map[string]liveCaptureFailure{record.Package: {origin: record.Origin, message: "permission denied"}}, nil)
	check("permission denied")
	cat.recordLiveScan(nil, nil)
	check("pkg currently offers 160.0,2")
}

func TestInventoryFindsNativeBuildBeforeRepositoryPublication(t *testing.T) {
	cfg := testutil.WorkerConfig(t.TempDir())
	day := time.Now().UTC().AddDate(0, 0, -1)
	finished := time.Date(day.Year(), day.Month(), day.Day(), 14, 43, 12, 0, time.UTC)
	started := finished.Add(-time.Hour)
	run := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports, started.Format(builder.BuildNameFormat))
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
	db, err := database.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := *db
	logs := builder.PackageLogHistory{Cfg: cfg, Save: store.RecordBuilds}
	if err := logs.Scan(t.Context(), func() builder.BuildStatus { return builder.BuildStatus{Started: started, Active: true} }); err != nil {
		t.Fatal(err)
	}
	builds, err := store.RecentInventoryBuilds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	view := ManagementPackages(nil, nil, nil, nil, nil, builds)
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
	db, err := database.Open(dir, false)
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
	db, err = database.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	builds, err := db.RecentInventoryBuilds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	view := ManagementPackages(nil, nil, nil, nil, nil, builds)
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
	build := database.BuildRecord{
		Package: "firefox-161.0,2", Origin: origin, ABI: abi, Started: finished.Add(-time.Hour),
		Finished: finished, Outcome: "succeeded", BuildSeconds: new(int64(1827)),
	}
	cached := packageinfo.Package{Name: "firefox", Origin: origin, BuildOrigin: origin, ABI: abi, Version: "159.0,2", Size: 11}
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
			local := packageinfo.Package{
				Name: "firefox", Version: tc.published, Origin: tc.origin, BuildOrigin: tc.origin, ABI: tc.abi,
				Local: true, Path: "All/firefox-" + tc.published + ".pkg", Size: 37,
			}
			snapshot := &Snapshot{
				ByPath: map[string]packageinfo.Package{local.Path: local}, PathByName: map[string]string{"firefox": local.Path},
				Compiled: map[string]packageinfo.Package{"firefox": local}, compiledAt: map[string]time.Time{"firefox": finished.Add(-24 * time.Hour)},
				Displaced: map[string]packageinfo.Package{"firefox": cached},
			}
			cache := []cachedArchive{{Key: "cached", Package: cached, Used: finished.Add(-2 * time.Hour)}}
			view := ManagementPackages(snapshot, nil, nil, cache, nil, []database.BuildRecord{build})
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

func TestPackageInventoryArchivesAndConsumers(t *testing.T) {
	local := packageinfo.Package{Name: "nginx", Origin: "www/nginx", BuildOrigin: "www/nginx", Version: "2.0", Size: 90_000_000, Path: "All/nginx-2.0.pkg", Local: true}
	upstream := local
	upstream.Version, upstream.Size, upstream.Local = "1.9", 88_000_000, false
	devel := packageinfo.Package{Name: "tool", Origin: "devel/tool", BuildOrigin: "devel/tool", Version: "3", Size: 999, Path: "All/tool-3.pkg"}
	s := &Snapshot{
		ByPath: map[string]packageinfo.Package{local.Path: local, devel.Path: devel}, PathByName: map[string]string{"nginx": local.Path, "tool": devel.Path},
		Displaced: map[string]packageinfo.Package{"nginx": upstream}, Compiled: map[string]packageinfo.Package{"nginx": local},
		compiledAt: map[string]time.Time{"nginx": time.Unix(20, 0)},
	}
	cached := upstream
	cached.Version, cached.Size = "1.8", 80_000_000
	entries := []cachedArchive{
		{Key: "old", Package: cached, Used: time.Unix(10, 0)},
		{Key: "unknown", Package: packageinfo.Package{Size: 45}, Used: time.Unix(12, 0)},
	}
	target := database.TrackedPackageRecord{Name: "nginx", Origin: "www/nginx", BuildOrigin: "www/nginx", Consumers: 10, LastSeen: 30}
	view := ManagementPackages(s, []database.TrackedPackageRecord{target}, []string{"devel/tool", "x11/unbuilt", "www/nginx"}, entries,
		map[string]builder.ResolvedPort{"nginx": {Origin: "www/nginx", Version: "2.1"}}, nil)
	if len(view.Packages) != 5 || view.CachedCount != 2 || view.CachedBytes != 80_000_045 || view.CompiledBytes == nil || *view.CompiledBytes != local.Size || view.TrackedOrigins != 3 || !view.CatalogReady || !view.OriginsReady {
		t.Fatalf("unexpected totals: %+v", view)
	}
	for _, p := range view.Packages {
		switch p.Name {
		case "nginx":
			if !p.Tracked || p.Consumers == nil || *p.Consumers != 10 || p.FreeBSDVersion != "1.9" || p.LocalVersion != "2.0" || p.PortVersion != "2.1" {
				t.Fatal(p)
			}
			if p.Compiled && (!p.VersionsDiffer || p.Source != "poudrine" || p.Cached || *p.Size != local.Size || p.LastSeen != 30) {
				t.Fatal("incorrect local archive", p)
			}
			if p.Cached && (p.VersionsDiffer || p.Source != "freebsd" || p.Compiled || p.CacheVersion != "1.8" || *p.Size != cached.Size || p.LastSeen != 10) {
				t.Fatal("incorrect cached archive", p)
			}
		case "tool":
			if !p.Tracked || p.Source != "freebsd" || p.Cached || p.Compiled || p.LocalVersion != "" || *p.Consumers != 0 || p.FreeBSDVersion != "3" || *p.Size != 999 {
				t.Fatal("configured origin lost catalog metadata", p)
			}
		case "":
			if p.Cached {
				if p.CacheKey != "unknown" || *p.Size != 45 || p.Consumers != nil {
					t.Fatal("unidentified cached archive", p)
				}
			} else if !p.Tracked || p.BuildOrigin != "x11/unbuilt" || p.Size != nil {
				t.Fatal("unresolved worker origin", p)
			}
		default:
			t.Fatal("unexpected row", p)
		}
	}
	cold := ManagementPackages(nil, nil, nil, entries, nil, nil)
	if cold.CatalogReady || cold.CompiledBytes != nil || cold.OriginsReady || cold.CachedCount != 2 || len(cold.Packages) != 2 {
		t.Fatalf("cache disappeared before first refresh: %+v", cold)
	}
}

func TestInventoryShowsPublishedWorkerFetch(t *testing.T) {
	const name, origin = "dav1d", "multimedia/dav1d"
	old := packageinfo.Package{Name: name, Origin: origin, BuildOrigin: origin, Version: "1.5.4_1", Size: 680000, Path: "All/dav1d-1.5.4_1.pkg", Local: true}
	upstream := old
	upstream.Local = false
	fetched := old
	fetched.Version, fetched.Size, fetched.Path = "1.5.5", 710000, "All/dav1d-1.5.5.pkg"
	s := &Snapshot{
		Compiled:   map[string]packageinfo.Package{name: old},
		Displaced:  map[string]packageinfo.Package{name: upstream},
		ByPath:     map[string]packageinfo.Package{fetched.Path: fetched},
		PathByName: map[string]string{name: fetched.Path},
		Fetched:    map[string]bool{name: true},
	}
	for _, origins := range [][]string{nil, {origin}} {
		view := ManagementPackages(s, nil, origins, nil, nil, nil)
		if len(view.Packages) != 2 || view.Packages[0].Version == view.Packages[1].Version ||
			view.CompiledBytes == nil || *view.CompiledBytes != old.Size || view.CachedBytes != 0 || view.CachedCount != 0 {
			t.Fatalf("unexpected archive rows or totals: %+v", view)
		}
		for _, row := range view.Packages {
			switch row.Version {
			case old.Version:
				if row.WorkerFetched || !row.Compiled || row.Source != "poudrine" {
					t.Fatalf("unexpected older compiled row: %+v", row)
				}
			case fetched.Version:
				if !row.WorkerFetched || row.Compiled || row.Cached || row.HistoryOnly || row.Source != "freebsd" ||
					row.Size == nil || *row.Size != fetched.Size || row.LocalVersion != old.Version || row.FreeBSDVersion != upstream.Version {
					t.Fatalf("unexpected published dependency row: %+v", row)
				}
			default:
				t.Fatalf("unexpected package: %+v", row)
			}
		}
	}
	cache := []cachedArchive{{Key: "cached-dav1d", Package: fetched, Used: time.Unix(42, 0)}}
	tracked := []database.TrackedPackageRecord{{Name: name, Origin: origin, Consumers: 1}}
	view := ManagementPackages(s, tracked, nil, cache, nil, nil)
	if len(view.Packages) != 2 || view.CachedCount != 1 || view.CachedBytes != fetched.Size {
		t.Fatalf("duplicate worker fetch or incorrect HTTP cache totals: %+v", view)
	}
	var combined bool
	for _, row := range view.Packages {
		if row.WorkerFetched && row.Cached && row.Tracked && row.CacheKey == cache[0].Key && row.LastSeen == 42 {
			combined = true
		}
	}
	if !combined {
		t.Fatalf("missing storage details for published worker fetch: %+v", view.Packages)
	}
	// A worker-fetched archive can be older than the upstream catalog.
	// Only include fetches that Poudrine actually offers to clients.
	s.ByPath = map[string]packageinfo.Package{upstream.Path: upstream}
	s.PathByName[name] = upstream.Path
	if view := ManagementPackages(s, nil, nil, nil, nil, nil); len(view.Packages) != 1 || view.Packages[0].WorkerFetched {
		t.Fatalf("unpublished worker fetch appeared in inventory: %+v", view)
	}
}

func TestInventoryMatchesPublishedBuildVersion(t *testing.T) {
	db, err := database.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := *db
	now := time.Now().UTC()
	const abi = "freebsd:15:amd64"
	const spec = "devel/example@default"
	records := []database.BuildRecord{
		{Package: "example-1", Outcome: "succeeded", BuildSeconds: new(int64(1))},
		{Package: "example-1", Outcome: "succeeded", BuildSeconds: new(int64(0))},
		{Package: "example-2", Outcome: "succeeded", BuildSeconds: new(int64(42))},
		{Package: "example-1", Outcome: "failed", BuildSeconds: new(int64(12))},
		{Package: "example-1", Outcome: "succeeded", Origin: "devel/example@other", BuildSeconds: new(int64(37))},
		{Package: "example-1", Outcome: "succeeded", ABI: "freebsd:15:i386", BuildSeconds: new(int64(9))},
	}
	for i := range records {
		r := &records[i]
		r.Started = now.Add(time.Duration(i) * time.Hour)
		r.Finished = r.Started.Add(time.Minute)
		if r.Origin == "" {
			r.Origin = spec
		}
		if r.ABI == "" {
			r.ABI = abi
		}
	}
	if err := store.RecordBuilds(t.Context(), records); err != nil {
		t.Fatal(err)
	}
	compiled := packageinfo.Package{Name: "example", Version: "1", ABI: abi, Origin: "devel/example", BuildOrigin: spec}
	snapshot := &Snapshot{Compiled: map[string]packageinfo.Package{"example": compiled}}
	upstream := compiled
	for _, tc := range []struct {
		version string
		differs bool
	}{{"3", true}, {"1.0", false}, {"", false}} {
		upstream.Version = tc.version
		snapshot.Displaced = map[string]packageinfo.Package{"example": upstream}
		view := ManagementPackages(snapshot, nil, nil, []cachedArchive{{Package: upstream}}, nil, nil)
		if err := LoadBuildTimes(t.Context(), store, snapshot, view.Packages); err != nil {
			t.Fatal(err)
		}
		for _, row := range view.Packages {
			if row.Compiled {
				if row.BuildSeconds == nil || *row.BuildSeconds != 0 || row.VersionsDiffer != tc.differs {
					t.Fatalf("incorrect compiled duration or comparison for FreeBSD %q: %+v", tc.version, row)
				}
			} else if row.BuildSeconds != nil || row.VersionsDiffer {
				t.Fatalf("cached archive received compiled data: %+v", row)
			}
		}
	}
}

func TestInventoryDurationOutlivesBuildHistory(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := *db
	at := time.Now().UTC().AddDate(-2, 0, 0)
	build := database.BuildRecord{Started: at, Finished: at.Add(time.Minute), Package: "example-1", Origin: "devel/example", Outcome: "succeeded", BuildSeconds: new(int64(48))}
	earlier := build
	earlier.Started = at.Add(-time.Hour)
	earlier.Finished = earlier.Started.Add(time.Minute)
	earlier.BuildSeconds = new(int64(31))
	recent := database.BuildRecord{Started: time.Now().UTC(), Finished: time.Now().UTC(), Package: "other-1", Outcome: "succeeded"}
	if _, err := db.Exec("INSERT INTO push_build_watch VALUES('test',0)"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []database.BuildRecord{build, recent, earlier, build} {
		if err := store.RecordBuilds(t.Context(), []database.BuildRecord{r}); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		"SELECT count(*) FROM package_builds WHERE package='example-1'",
		"SELECT count(*) FROM package_build_times WHERE package='example-1'",
		"SELECT count(*) FROM push_build_queue WHERE package='example-1'",
	} {
		var count int
		if err := db.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: count=%d err=%v", query, count, err)
		}
	}
	reader, err := database.Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	local := packageinfo.Package{Name: "example", Version: "1", Origin: "devel/example"}
	snapshot := &Snapshot{Compiled: map[string]packageinfo.Package{"example": local}}
	view := ManagementPackages(snapshot, nil, nil, nil, nil, nil)
	if err := LoadBuildTimes(t.Context(), *reader, snapshot, view.Packages); err != nil {
		t.Fatal(err)
	}
	if len(view.Packages) != 1 || view.Packages[0].BuildSeconds == nil || *view.Packages[0].BuildSeconds != 48 {
		t.Fatalf("lost the latest duration of an older published archive: %+v", view.Packages)
	}
}

func TestInventoryUsesDurationOfLatestMatchingBuild(t *testing.T) {
	db, err := database.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := *db
	now := time.Now().UTC()
	pkg := packageinfo.Package{Name: "firefox", Version: "161.0,2", Origin: "www/firefox"}
	old := database.BuildRecord{Started: now.Add(-2 * time.Hour), Finished: now.Add(-time.Hour), Package: "firefox-161.0,2",
		Origin: pkg.Origin, Outcome: "succeeded", BuildSeconds: new(int64(600))}
	if err := store.RecordBuilds(t.Context(), []database.BuildRecord{old}); err != nil {
		t.Fatal(err)
	}
	// This later result was retained without timing metadata.
	if _, err := db.Exec("INSERT INTO package_builds VALUES(?, ?, 'succeeded', ?)", now.Add(-time.Minute).UnixNano(), old.Package, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	builds, err := store.RecentInventoryBuilds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snap := &Snapshot{Compiled: map[string]packageinfo.Package{pkg.Name: pkg}}
	view := ManagementPackages(snap, nil, nil, nil, nil, builds)
	if err := LoadBuildTimes(t.Context(), store, snap, view.Packages); err != nil {
		t.Fatal(err)
	}
	if len(view.Packages) != 1 || view.Packages[0].BuildSeconds != nil || view.Packages[0].LastSeen != now.Unix() {
		t.Fatalf("expected the newest build without a duration: %+v", view.Packages)
	}
}
