package poudrine

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestCacheListsArchiveAfterRestartAndEviction(t *testing.T) {
	var compressed bytes.Buffer
	z, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(z)
	manifest := []byte(`{"name":"wheel","origin":"devel/wheel","version":"1","annotations":{"flavor":"py312","subpackage":"docs"}}`)
	if err := w.WriteHeader(&tar.Header{Name: "+COMPACT_MANIFEST", Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(manifest); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/All/wheel.pkg" {
			w.Write(compressed.Bytes())
			return
		}
		fmt.Fprint(w, "x")
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	p := testPackage("wheel", compressed.String())
	p.BuildOrigin = "devel/wheel@py312~docs"
	openCache := func() *packageCache {
		t.Helper()
		c, err := newPackageCache(dir, p.Size, u, up.Client())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	cache := openCache()
	get := func(p CatalogPackage) {
		t.Helper()
		f, err := cache.open(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	check := func() {
		t.Helper()
		list := cache.inventory(nil)
		if len(list) != 1 {
			t.Fatal(list)
		}
		got := list[0]
		if got.Package.Name != p.Name || got.Package.Version != p.Version || got.Package.BuildOrigin != p.BuildOrigin ||
			got.Package.Size != p.Size || got.Key != packageCacheKey(p.Sum) || got.Used.IsZero() {
			t.Fatalf("incorrect archive entry: %+v", got)
		}
	}
	get(p)
	check()
	cache = openCache()
	check() // The upstream no longer needs to list this version.
	get(testPackage("replacement", "x"))
	if list := cache.inventory(nil); len(list) != 1 || list[0].Package.Name != "replacement" {
		t.Fatal("evicted archive still present", list)
	}
}

func TestLegacyCacheIdentityFromCatalog(t *testing.T) {
	p := testPackage("wheel", "opaque legacy archive")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, packageCacheKey(p.Sum)), []byte("opaque legacy archive"), 0600); err != nil {
		t.Fatal(err)
	}
	cache, err := newPackageCache(dir, p.Size, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if list := cache.inventory(nil); len(list) != 1 || list[0].Package.Name != "" || list[0].Package.Size != p.Size {
		t.Fatal("missing unrecognized archive", list)
	}
	s := &catalogSnapshot{pathByName: map[string]string{p.Name: "All/local.pkg"}, displaced: map[string]CatalogPackage{p.Name: p}}
	if list := cache.inventory(s); len(list) != 1 || list[0].Package != p {
		t.Fatal("legacy archive did not match upstream digest", list)
	}
	if cache.inventory(nil)[0].Package != p {
		t.Fatal("lost archive identity after catalog change")
	}
}

func compiledFixture(t *testing.T, p CatalogPackage, body string) *catalogSnapshot {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compiled.pkg")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return &catalogSnapshot{
		compiled:      map[string]CatalogPackage{p.Name: p},
		compiledFiles: map[string]*compiledArchive{p.Name: {path: path, info: stat}},
	}
}

func TestPruneCompiledCache(t *testing.T) {
	for _, tc := range []struct {
		label, cachedVersion, localVersion, localABI, localFlavor, localBytes string
		remove                                                                bool
	}{
		{"equal", "1.0", "1", "freebsd:15:amd64", "one", "local", true},
		{"newer epoch", "9_3", "1,1", "freebsd:15:*", "one", "local", true},
		{"older revision", "2_2", "2_1", "freebsd:15:amd64", "one", "local", false},
		{"different flavor", "1", "2", "freebsd:15:amd64", "two", "local", false},
		{"different ABI", "1", "2", "freebsd:14:amd64", "one", "local", false},
		{"corrupt local", "1", "2", "freebsd:15:amd64", "one", "wrong", false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			const bytes = "from FreeBSD"
			p := testPackage("wheel", bytes)
			p.Version, p.ABI, p.BuildOrigin = tc.cachedVersion, "freebsd:15:amd64", p.Origin+"@one"
			dir := t.TempDir()
			path := filepath.Join(dir, packageCacheKey(p.Sum))
			if err := os.WriteFile(path, []byte(bytes), 0600); err != nil {
				t.Fatal(err)
			}
			cache, err := newPackageCache(dir, 1024, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			local := testPackage("wheel", "local")
			local.Version, local.BuildOrigin, local.ABI = tc.localVersion, local.Origin+"@"+tc.localFlavor, tc.localABI
			s := compiledFixture(t, local, tc.localBytes)
			s.pathByName = map[string]string{p.Name: p.Path}
			s.displaced = map[string]CatalogPackage{p.Name: p}
			// The old cache was reopened with no manifest identity. The signed
			// displaced entry supplies the identity before the first prune.
			if err := cache.pruneCompiled(s); err != nil {
				t.Fatal(err)
			}
			list := cache.inventory(s)
			_, statErr := os.Stat(path)
			if tc.remove {
				if len(list) != 0 || cache.bytes != 0 || !os.IsNotExist(statErr) {
					t.Fatalf("redundant archive survived: %v, %d bytes, stat %v", list, cache.bytes, statErr)
				}
			} else if len(list) != 1 || list[0].Package != p || cache.bytes != p.Size || statErr != nil {
				t.Fatalf("lost retained archive: %v, %d bytes, stat %v", list, cache.bytes, statErr)
			}
		})
	}
}

func TestCacheDownloadDuringLocalPublication(t *testing.T) {
	const bytes = "from FreeBSD"
	p := testPackage("wheel", bytes)
	local := testPackage("wheel", "local")
	started, finish := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-finish
		fmt.Fprint(w, bytes)
	}))
	defer server.Close()
	upstream, _ := url.Parse(server.URL)
	cache, err := newPackageCache(t.TempDir(), 1024, upstream, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	s := compiledFixture(t, local, "local")
	done := make(chan error, 1)
	go func() {
		f, err := cache.open(t.Context(), p)
		if err == nil {
			defer f.Close()
			var body []byte
			body, err = io.ReadAll(f)
			if err == nil && string(body) != bytes {
				err = fmt.Errorf("served stale request with wrong bytes: %q", body)
			}
		}
		done <- err
	}()
	<-started
	err = cache.pruneCompiled(s)
	close(finish)
	if fetchErr := <-done; err != nil || fetchErr != nil {
		t.Fatalf("prune %v, fetch %v", err, fetchErr)
	}
	if list := cache.inventory(s); len(list) != 0 || cache.bytes != 0 {
		t.Fatalf("late download repopulated cache: %v", list)
	}
	// If the compiled archive disappears, an upstream request may again be cached.
	if err := os.Remove(s.compiledFiles[p.Name].path); err != nil || s.supersedes(p) {
		t.Fatalf("missing local archive still overrides upstream: %v", err)
	}
}
