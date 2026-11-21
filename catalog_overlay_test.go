package poudrine

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
)

func TestCatalogOverlayProvenance(t *testing.T) {
	const origin = "devel/fixture"
	overlay := PackageOverlay{OptionsSet: []string{"CUSTOM"}}
	digest := overlay.digest()
	state := t.TempDir()
	trust, local := filepath.Join(state, "keys"), filepath.Join(state, "packages")
	c, err := NewCatalog(state, "https://repo.invalid/", trust, local, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.overlays = PackageOverlays{origin: overlay}
	revision := catalogRevision{fingerprint: strings.Repeat("1", 64)}
	putLiveFixture(t, filepath.Join(trust, "trusted", "repo"), []byte("function: \"sha256\"\nfingerprint: \""+revision.fingerprint+"\"\n"))
	rawManifest := func(name, version, marker string) string {
		m := strings.TrimSuffix(liveFixtureOriginManifest(name, origin, version), "}")
		return fmt.Sprintf(`%s,"annotations":{"flavor":"custom","subpackage":%q,%q:%q}}`, m, name, overlayAnnotation, marker)
	}

	// Every upstream package, including the one bearing our annotation, is
	// unavailable unless a matching local build supplies that package.
	var stock, native [][]byte
	live := map[string]livePackage{}
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
		b := liveFixtureArchive(t, m)
		if name == "mislabeled" {
			b = liveFixtureArchive(t, rawManifest(name, "1", ""))
		}
		if example.staged {
			sum := fmt.Sprintf("%x", sha256.Sum256(b))
			path := filepath.Join(c.liveDir, sum+".pkg")
			putLiveFixture(t, path, b)
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := readLiveManifest(f, sum)
			info, statErr := f.Stat()
			f.Close()
			if err != nil || statErr != nil {
				t.Fatalf("read fixture: %v, %v", err, statErr)
			}
			live[name] = livePackage{manifest: parsed, file: &compiledArchive{path: path, info: info}, fetched: example.fetched}
			continue
		}
		path := "All/" + name + "-1.pkg"
		putLiveFixture(t, filepath.Join(local, path), b)
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
	stock = append(stock, liveFixtureCatalogEntry(liveFixtureManifest("ordinary", "2"), "All/ordinary-2.pkg", []byte("stock")))
	upstream, err := parseCatalog(bytes.Join(stock, []byte("\n")), "packagesite.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := signedPkg(c.key, "packagesite.yaml", bytes.Join(native, []byte("\n")))
	if err != nil {
		t.Fatal(err)
	}
	putLiveFixture(t, filepath.Join(local, "packagesite.pkg"), archive)
	f, info, err := c.openLocalIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := c.publishCatalog(t.Context(), &upstream, revision, f, info, live); err != nil {
		t.Fatal(err)
	}
	s := c.current.Load()
	if len(s.byPath) != 3 || len(s.compiled) != 2 || s.ahead != 0 {
		t.Fatalf("unexpected offered packages: %+v", s.byPath)
	}
	if p, ok := c.Package("All/ordinary-2.pkg"); !ok || p.Local {
		t.Fatalf("unrelated upstream package: %+v", p)
	}
	for _, name := range []string{"compiled", "live-compiled"} {
		built := s.compiled[name]
		stock, ok := s.freeBSDPackage(name)
		if built.overlayDigest != digest || built.Version != "1" || !ok || stock.Version != "2" {
			t.Fatalf("compiled and FreeBSD versions for %s: %+v %+v", name, built, stock)
		}
	}

	db, err := openInventory(state, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := repository{catalog: c, store: inventory{db}, localDir: local}
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
	check(s.pathByName["live-compiled"], 200)
	check(live["live-fetched"].manifest.pkg.Path, 404)
	check(live["live-stock"].manifest.pkg.Path, 404)
	for _, name := range []string{"compiled", "mislabeled", "old-settings", "fetched", "pkg", "live-compiled", "live-fetched", "live-stock"} {
		check("All/"+name+"-2.pkg", 404)
	}
	for _, path := range []string{"All/old-settings-1.pkg", "All/mislabeled-1.pkg", "All/fetched-1.pkg", "All/pkg-1.pkg", "Latest/pkg.pkg", "Latest/pkg.pkg.pubkeysig"} {
		check(path, 404)
	}

	// Old content-addressed URLs survive a restart on disk. The archive's
	// own manifest must still satisfy the new policy on every download.
	c.current.Store(nil)
	check(live["live-compiled"].manifest.pkg.Path, 503)
	c.current.Store(&catalogSnapshot{overlays: c.overlays, displaced: s.displaced})
	for _, name := range []string{"live-stock", "live-fetched"} {
		staged := live[name]
		target := filepath.Join(c.liveDir, "retired", filepath.Base(staged.manifest.pkg.Path))
		b, err := os.ReadFile(staged.file.path)
		if err != nil {
			t.Fatal(err)
		}
		putLiveFixture(t, target, b)
	}
	// A port can move between collections. A pinned URL for the old origin
	// must not supply the binary now reserved for the configured overlay.
	moved := liveFixtureArchive(t, liveFixtureOriginManifest("old-settings", "devel/former-port", "0"))
	movedFile := fmt.Sprintf("%x.pkg", sha256.Sum256(moved))
	putLiveFixture(t, filepath.Join(c.liveDir, "retired", movedFile), moved)
	check(liveURLPrefix+movedFile, 404)
	c.live = nil
	if err := pruneLiveDownloads(c, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	check(live["live-stock"].manifest.pkg.Path, 404)
	check(live["live-fetched"].manifest.pkg.Path, 404)
	check(live["live-compiled"].manifest.pkg.Path, 200)
	c.overlays = PackageOverlays{origin: {OptionsUnset: []string{"CUSTOM"}}}
	c.current.Store(&catalogSnapshot{overlays: c.overlays, displaced: s.displaced})
	check(live["live-compiled"].manifest.pkg.Path, 404)
	c.overlays = nil
	c.current.Store(nil)
	check(live["live-compiled"].manifest.pkg.Path, 404)

	// An old proxy cache entry for a newly overlaid origin can be removed
	// even before the first custom archive is available.
	cacheDir := t.TempDir()
	p := upstream.byName["old-settings"].pkg
	putLiveFixture(t, filepath.Join(cacheDir, packageCacheKey(p.Sum)), []byte(p.Name))
	cache, err := newPackageCache(cacheDir, 1024, c.upstream, c.client)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.pruneCompiled(s); err != nil || cache.bytes != 0 {
		t.Fatalf("stock bytes left in cache: %d, err: %v", cache.bytes, err)
	}
}
