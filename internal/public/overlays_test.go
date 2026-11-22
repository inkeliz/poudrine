package public

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestCatalogOverlayProvenance(t *testing.T) {
	const origin = "devel/fixture"
	overlay := config.PackageOverlay{OptionsSet: []string{"CUSTOM"}}
	digest := overlay.Digest()
	state := t.TempDir()
	trust, local := filepath.Join(state, "keys"), filepath.Join(state, "packages")
	c, err := catalog.New(state, "https://repo.invalid/", trust, local, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Overlays = config.PackageOverlays{origin: overlay}
	revision := catalog.CatalogRevision{Fingerprint: strings.Repeat("1", 64)}
	testutil.WriteFile(t, filepath.Join(trust, "trusted", "repo"), []byte("function: \"sha256\"\nfingerprint: \""+revision.Fingerprint+"\"\n"))
	rawManifest := func(name, version, marker string) string {
		m := strings.TrimSuffix(testutil.OriginManifest(name, origin, version), "}")
		return fmt.Sprintf(`%s,"annotations":{"flavor":"custom","subpackage":%q,%q:%q}}`, m, name, config.OverlayAnnotation, marker)
	}

	// Every upstream package, including the one bearing our annotation, is
	// unavailable unless a matching local build supplies that package.
	var stock, native [][]byte
	live := map[string]catalog.LivePackage{}
	for _, example := range []struct {
		name, marker    string
		staged, fetched bool
	}{
		{"compiled", digest, false, false},
		{"mislabeled", digest, false, false},
		{"old-settings", strings.Repeat("0", 64), false, false},
		{"pkg", "", false, false},
		{"fetched", digest, false, true},
		{"live-compiled", digest, true, false},
		{"live-fetched", digest, true, true},
		{"live-stock", "", true, false},
	} {
		name := example.name
		stockManifest := rawManifest(name, "2", digest)
		stock = append(stock, liveFixtureCatalogEntry(stockManifest, "All/"+name+"-2.pkg", []byte(name)))
		m := rawManifest(name, "1", example.marker)
		b := testutil.Archive(t, m)
		if name == "mislabeled" {
			b = testutil.Archive(t, rawManifest(name, "1", ""))
		}
		if example.staged {
			sum := fmt.Sprintf("%x", sha256.Sum256(b))
			path := filepath.Join(c.LiveDir, sum+".pkg")
			testutil.WriteFile(t, path, b)
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := catalog.ReadLiveManifest(f, sum)
			info, statErr := f.Stat()
			f.Close()
			if err != nil || statErr != nil {
				t.Fatalf("read fixture: %v, %v", err, statErr)
			}
			live[name] = catalog.LivePackage{Manifest: parsed, File: &catalog.CompiledArchive{Path: path, Info: info}, Fetched: example.fetched}
			continue
		}
		path := "All/" + name + "-1.pkg"
		testutil.WriteFile(t, filepath.Join(local, path), b)
		native = append(native, liveFixtureCatalogEntry(m, path, b))
		if example.fetched {
			dir := filepath.Join(local, ".pkg-cache")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(filepath.Join(local, path), filepath.Join(dir, filepath.Base(path))); err != nil {
				t.Fatal(err)
			}
		}
	}
	stock = append(stock, liveFixtureCatalogEntry(testutil.Manifest("ordinary", "2"), "All/ordinary-2.pkg", []byte("stock")))
	upstream, err := catalog.ParseCatalog(bytes.Join(stock, []byte("\n")), "packagesite.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := catalog.SignedPkg(c.Key, "packagesite.yaml", bytes.Join(native, []byte("\n")))
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(local, "packagesite.pkg"), archive)
	f, info, err := c.OpenLocalIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := c.PublishCatalog(t.Context(), &upstream, revision, f, info, live); err != nil {
		t.Fatal(err)
	}
	s := c.Current.Load()
	if len(s.ByPath) != 3 || len(s.Compiled) != 2 || s.Ahead != 0 {
		t.Fatalf("unexpected offered packages: %+v", s.ByPath)
	}
	if p, ok := c.Package("All/ordinary-2.pkg"); !ok || p.Local {
		t.Fatalf("unrelated upstream package: %+v", p)
	}
	for _, name := range []string{"compiled", "live-compiled"} {
		built := s.Compiled[name]
		stock, ok := s.FreeBSDPackage(name)
		if built.OverlayDigest != digest || built.Version != "1" || !ok || stock.Version != "2" {
			t.Fatalf("compiled and FreeBSD versions for %s: %+v %+v", name, built, stock)
		}
	}

	db, err := database.Open(state, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := Repository{Catalog: c, Store: (*db), LocalDir: local}
	check := func(path string, status int) {
		t.Helper()
		for _, method := range []string{"HEAD", "GET"} {
			w := httptest.NewRecorder()
			repo.ServeHTTP(w, httptest.NewRequest(method, "/"+path, nil))
			if w.Code != status {
				t.Errorf("%s %s: got %d, want %d", method, path, w.Code, status)
			}
		}
	}
	check("All/compiled-1.pkg", 200)
	check(s.PathByName["live-compiled"], 200)
	check(live["live-fetched"].Manifest.Pkg.Path, 404)
	check(live["live-stock"].Manifest.Pkg.Path, 404)
	for _, name := range []string{"compiled", "mislabeled", "old-settings", "fetched", "pkg", "live-compiled", "live-fetched", "live-stock"} {
		check("All/"+name+"-2.pkg", 404)
	}
	for _, path := range []string{"All/old-settings-1.pkg", "All/mislabeled-1.pkg", "All/fetched-1.pkg", "All/pkg-1.pkg", "Latest/pkg.pkg", "Latest/pkg.pkg.pubkeysig"} {
		check(path, 404)
	}

	// Old content-addressed URLs survive a restart on disk. The archive's
	// own manifest must still satisfy the new policy on every download.
	c.Current.Store(nil)
	check(live["live-compiled"].Manifest.Pkg.Path, 503)
	c.Current.Store(&catalog.Snapshot{Overlays: c.Overlays, Displaced: s.Displaced})
	for _, name := range []string{"live-stock", "live-fetched"} {
		staged := live[name]
		target := filepath.Join(c.LiveDir, "retired", filepath.Base(staged.Manifest.Pkg.Path))
		b, err := os.ReadFile(staged.File.Path)
		if err != nil {
			t.Fatal(err)
		}
		testutil.WriteFile(t, target, b)
	}
	// A port can move between collections. A pinned URL for the old origin
	// must not supply the binary now reserved for the configured overlay.
	moved := testutil.Archive(t, testutil.OriginManifest("old-settings", "devel/former-port", "0"))
	movedFile := fmt.Sprintf("%x.pkg", sha256.Sum256(moved))
	testutil.WriteFile(t, filepath.Join(c.LiveDir, "retired", movedFile), moved)
	check(catalog.LiveURLPrefix+movedFile, 404)
	c.Live = nil
	if err := catalog.PruneLiveDownloads(c, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	check(live["live-stock"].Manifest.Pkg.Path, 404)
	check(live["live-fetched"].Manifest.Pkg.Path, 404)
	check(live["live-compiled"].Manifest.Pkg.Path, 200)
	c.Overlays = config.PackageOverlays{origin: {OptionsUnset: []string{"CUSTOM"}}}
	c.Current.Store(&catalog.Snapshot{Overlays: c.Overlays, Displaced: s.Displaced})
	check(live["live-compiled"].Manifest.Pkg.Path, 404)
	c.Overlays = nil
	c.Current.Store(nil)
	check(live["live-compiled"].Manifest.Pkg.Path, 404)

	// An old proxy cache entry for a newly overlaid origin can be removed
	// even before the first custom archive is available.
	cacheDir := t.TempDir()
	p := upstream.ByName["old-settings"].Pkg
	testutil.WriteFile(t, filepath.Join(cacheDir, catalog.PackageCacheKey(p.Sum)), []byte(p.Name))
	cache, err := catalog.NewCache(cacheDir, 1024, c.Upstream, c.Client)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.PruneCompiled(s); err != nil || cache.Bytes != 0 {
		t.Fatalf("stock bytes left in cache: %d, err: %v", cache.Bytes, err)
	}
}
