package poudrine

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPackageInventoryArchivesAndConsumers(t *testing.T) {
	local := CatalogPackage{Name: "nginx", Origin: "www/nginx", BuildOrigin: "www/nginx", Version: "2.0", Size: 90_000_000, Path: "All/nginx-2.0.pkg", Local: true}
	upstream := local
	upstream.Version, upstream.Size, upstream.Local = "1.9", 88_000_000, false
	devel := CatalogPackage{Name: "tool", Origin: "devel/tool", BuildOrigin: "devel/tool", Version: "3", Size: 999, Path: "All/tool-3.pkg"}
	s := &catalogSnapshot{
		byPath: map[string]CatalogPackage{local.Path: local, devel.Path: devel}, pathByName: map[string]string{"nginx": local.Path, "tool": devel.Path},
		displaced: map[string]CatalogPackage{"nginx": upstream}, compiled: map[string]CatalogPackage{"nginx": local},
		compiledAt: map[string]time.Time{"nginx": time.Unix(20, 0)},
	}
	cached := upstream
	cached.Version, cached.Size = "1.8", 80_000_000
	entries := []cachedArchive{
		{Key: "old", Package: cached, Used: time.Unix(10, 0)},
		{Key: "unknown", Package: CatalogPackage{Size: 45}, Used: time.Unix(12, 0)},
	}
	target := trackedPackageRecord{Name: "nginx", Origin: "www/nginx", BuildOrigin: "www/nginx", Consumers: 10, LastSeen: 30}
	view := managementPackages(s, []trackedPackageRecord{target}, []string{"devel/tool", "x11/unbuilt", "www/nginx"}, entries,
		map[string]resolvedPort{"nginx": {"www/nginx", "2.1"}}, nil)
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
	cold := managementPackages(nil, nil, nil, entries, nil, nil)
	if cold.CatalogReady || cold.CompiledBytes != nil || cold.OriginsReady || cold.CachedCount != 2 || len(cold.Packages) != 2 {
		t.Fatalf("cache disappeared before first refresh: %+v", cold)
	}
}

func TestInventoryMatchesPublishedBuildVersion(t *testing.T) {
	db, err := openInventory(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := inventory{db}
	now := time.Now().UTC()
	const abi = "freebsd:15:amd64"
	const spec = "devel/example@default"
	records := []BuildRecord{
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
	if err := store.recordBuilds(t.Context(), records); err != nil {
		t.Fatal(err)
	}
	compiled := CatalogPackage{Name: "example", Version: "1", ABI: abi, Origin: "devel/example", BuildOrigin: spec}
	snapshot := &catalogSnapshot{compiled: map[string]CatalogPackage{"example": compiled}}
	upstream := compiled
	for _, tc := range []struct {
		version string
		differs bool
	}{{"3", true}, {"1.0", false}, {"", false}} {
		upstream.Version = tc.version
		snapshot.displaced = map[string]CatalogPackage{"example": upstream}
		view := managementPackages(snapshot, nil, nil, []cachedArchive{{Package: upstream}}, nil, nil)
		if err := store.loadBuildTimes(t.Context(), snapshot, view.Packages); err != nil {
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
	db, err := openInventory(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := inventory{db}
	at := time.Now().UTC().AddDate(-2, 0, 0)
	build := BuildRecord{Started: at, Finished: at.Add(time.Minute), Package: "example-1", Origin: "devel/example", Outcome: "succeeded", BuildSeconds: new(int64(48))}
	earlier := build
	earlier.Started = at.Add(-time.Hour)
	earlier.Finished = earlier.Started.Add(time.Minute)
	earlier.BuildSeconds = new(int64(31))
	recent := BuildRecord{Started: time.Now().UTC(), Finished: time.Now().UTC(), Package: "other-1", Outcome: "succeeded"}
	if _, err := db.Exec("INSERT INTO push_build_watch VALUES('test',0)"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []BuildRecord{build, recent, earlier, build} {
		if err := store.recordBuilds(t.Context(), []BuildRecord{r}); err != nil {
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
	reader, err := openInventory(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	local := CatalogPackage{Name: "example", Version: "1", Origin: "devel/example"}
	snapshot := &catalogSnapshot{compiled: map[string]CatalogPackage{"example": local}}
	view := managementPackages(snapshot, nil, nil, nil, nil, nil)
	if err := (inventory{reader}).loadBuildTimes(t.Context(), snapshot, view.Packages); err != nil {
		t.Fatal(err)
	}
	if len(view.Packages) != 1 || view.Packages[0].BuildSeconds == nil || *view.Packages[0].BuildSeconds != 48 {
		t.Fatalf("lost the latest duration of an older published archive: %+v", view.Packages)
	}
}

func TestInventoryUsesDurationOfLatestMatchingBuild(t *testing.T) {
	db, err := openInventory(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := inventory{db}
	now := time.Now().UTC()
	pkg := CatalogPackage{Name: "firefox", Version: "161.0,2", Origin: "www/firefox"}
	old := BuildRecord{Started: now.Add(-2 * time.Hour), Finished: now.Add(-time.Hour), Package: "firefox-161.0,2",
		Origin: pkg.Origin, Outcome: "succeeded", BuildSeconds: new(int64(600))}
	if err := store.recordBuilds(t.Context(), []BuildRecord{old}); err != nil {
		t.Fatal(err)
	}
	// This later result was retained without timing metadata.
	if _, err := db.Exec("INSERT INTO package_builds VALUES(?, ?, 'succeeded', ?)", now.Add(-time.Minute).UnixNano(), old.Package, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	builds, err := store.recentInventoryBuilds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snap := &catalogSnapshot{compiled: map[string]CatalogPackage{pkg.Name: pkg}}
	view := managementPackages(snap, nil, nil, nil, nil, builds)
	if err := store.loadBuildTimes(t.Context(), snap, view.Packages); err != nil {
		t.Fatal(err)
	}
	if len(view.Packages) != 1 || view.Packages[0].BuildSeconds != nil || view.Packages[0].LastSeen != now.Unix() {
		t.Fatalf("expected the newest build without a duration: %+v", view.Packages)
	}
}

func TestResolvedPortVersions(t *testing.T) {
	cfg := workerTestConfig(t.TempDir())
	bulk := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports)
	dir := filepath.Join(bulk, "test-build")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("test-build", filepath.Join(bulk, "latest")); err != nil {
		t.Fatal(err)
	}
	const spec = "devel/py-thing@py311"
	if err := os.WriteFile(filepath.Join(dir, ".poudriere.ports.queued"), []byte(spec+" py311-my-thing-2.1_2,1 listed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if p := resolvedPorts(cfg)["py311-my-thing"]; p != (resolvedPort{spec, "2.1_2,1"}) {
		t.Fatal(p)
	}
	if err := os.WriteFile(filepath.Join(dir, ".poudriere.all_pkgs%"), []byte("py311-my-thing-2.2 "+spec+" listed\ninvalid\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if p := resolvedPorts(cfg)["py311-my-thing"]; p != (resolvedPort{spec, "2.2"}) {
		t.Fatal(p)
	}
}

func TestPackageInventoryWithSupervisedOrigins(t *testing.T) {
	a := testAdmin(t)
	key := enrollAdmin(t, a)
	cfg := workerTestConfig(t.TempDir())
	if err := os.WriteFile(cfg.PackagesFile, []byte("www/nginx\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := NewBuilder(cfg, func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.status.PoudriereBytes = new(int64(8192))
	a.cfg.Packages = func(_ context.Context, origins []string) (packageInventoryView, error) {
		return managementPackages(nil, nil, origins, nil, nil, nil), nil
	}
	a, _ = supervisedTestAdmin(t, a.cfg, key, b)
	adminResponse(t, a, httptest.NewRequest(methodQuery, packagesPath, nil), 401)
	req := signedTestRequest(t, key, methodQuery, packagesPath, []byte("{}"), time.Now())
	var result packageInventoryView
	if err := json.Unmarshal(adminResponse(t, a, req, 200).Body.Bytes(), &result); err != nil || !result.OriginsReady || result.TrackedOrigins != 1 || len(result.Packages) != 1 || result.Packages[0].Origin != "www/nginx" {
		t.Fatalf("signed origin lookup: %+v (%v)", result, err)
	}
	if result.PoudriereBytes == nil || *result.PoudriereBytes != 8192 {
		t.Fatalf("missing worker disk usage: %+v", result)
	}
}
