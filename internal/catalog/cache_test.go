package catalog

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/testutil"
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
	p := testutil.Package("wheel", compressed.String())
	p.BuildOrigin = "devel/wheel@py312~docs"
	openCache := func() *Cache {
		t.Helper()
		c, err := NewCache(dir, p.Size, u, up.Client())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	cache := openCache()
	get := func(p packageinfo.Package) {
		t.Helper()
		f, err := cache.Open(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	check := func() {
		t.Helper()
		list := cache.Inventory(nil)
		if len(list) != 1 {
			t.Fatal(list)
		}
		got := list[0]
		if got.Package.Name != p.Name || got.Package.Version != p.Version || got.Package.BuildOrigin != p.BuildOrigin ||
			got.Package.Size != p.Size || got.Key != PackageCacheKey(p.Sum) || got.Used.IsZero() {
			t.Fatalf("incorrect archive entry: %+v", got)
		}
	}
	get(p)
	check()
	cache = openCache()
	check() // The upstream no longer needs to list this version.
	get(testutil.Package("replacement", "x"))
	if list := cache.Inventory(nil); len(list) != 1 || list[0].Package.Name != "replacement" {
		t.Fatal("evicted archive still present", list)
	}
}

func TestLegacyCacheIdentityFromCatalog(t *testing.T) {
	p := testutil.Package("wheel", "opaque legacy archive")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PackageCacheKey(p.Sum)), []byte("opaque legacy archive"), 0600); err != nil {
		t.Fatal(err)
	}
	cache, err := NewCache(dir, p.Size, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if list := cache.Inventory(nil); len(list) != 1 || list[0].Package.Name != "" || list[0].Package.Size != p.Size {
		t.Fatal("missing unrecognized archive", list)
	}
	s := &Snapshot{PathByName: map[string]string{p.Name: "All/local.pkg"}, Displaced: map[string]packageinfo.Package{p.Name: p}}
	if list := cache.Inventory(s); len(list) != 1 || list[0].Package != p {
		t.Fatal("legacy archive did not match upstream digest", list)
	}
	if cache.Inventory(nil)[0].Package != p {
		t.Fatal("lost archive identity after catalog change")
	}
}

func TestCachePruneUsesPublishedPackage(t *testing.T) {
	const body = "from FreeBSD"
	upstream := testutil.Package("wheel", body)
	for _, tc := range []struct {
		name         string
		localBody    string
		publishLocal bool
	}{
		{"upstream selected despite local build", "local", false},
		{"local selected with upstream retained for display", "local", true},
		{"local selected with identical checksum", body, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			key := PackageCacheKey(upstream.Sum)
			path := filepath.Join(dir, key)
			testutil.WriteFile(t, path, []byte(body))
			cache, err := NewCache(dir, 1024, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			local := testutil.Package("wheel", tc.localBody)
			s := cachePublication(upstream, local, tc.publishLocal)
			if err := cache.Prune(s); err != nil {
				t.Fatal(err)
			}
			list := cache.Inventory(s)
			_, err = os.Stat(path)
			if tc.publishLocal {
				if len(list) != 0 || cache.Bytes != 0 || !os.IsNotExist(err) {
					t.Fatalf("cache retained displaced upstream: entries %v, bytes %d, stat %v", list, cache.Bytes, err)
				}
			} else if len(list) != 1 || list[0].Package != upstream || cache.Bytes != upstream.Size || err != nil {
				t.Fatalf("lost selected upstream: entries %v, bytes %d, stat %v", list, cache.Bytes, err)
			}
		})
	}
}

func TestCacheDownloadDuringLocalPublication(t *testing.T) {
	const bytes = "from FreeBSD"
	p := testutil.Package("wheel", bytes)
	local := testutil.Package("wheel", "local")
	started, finish := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
			<-finish
		}
		fmt.Fprint(w, bytes)
	}))
	defer server.Close()
	upstream, _ := url.Parse(server.URL)
	cache, err := NewCache(t.TempDir(), 1024, upstream, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	publishedUpstream := cachePublication(p, local, false)
	if err := cache.Prune(publishedUpstream); err != nil {
		t.Fatal(err)
	}
	publishedLocal := cachePublication(p, local, true)
	done := make(chan error, 1)
	go func() {
		f, err := cache.Open(t.Context(), p)
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
	err = cache.Prune(publishedLocal)
	close(finish)
	if fetchErr := <-done; err != nil || fetchErr != nil {
		t.Fatalf("prune %v, fetch %v", err, fetchErr)
	}
	if list := cache.Inventory(publishedLocal); len(list) != 0 || cache.Bytes != 0 {
		t.Fatalf("late download repopulated cache: %v", list)
	}
	if err := cache.Prune(publishedUpstream); err != nil {
		t.Fatal(err)
	}
	f, err := cache.Open(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if list := cache.Inventory(publishedUpstream); len(list) != 1 || list[0].Package != p || cache.Bytes != p.Size || requests.Load() != 2 {
		t.Fatalf("republished upstream not cached: entries %v, bytes %d, requests %d", list, cache.Bytes, requests.Load())
	}
}

func TestCacheLRUAndIntegrity(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/All/"), ".pkg"))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	cache, err := NewCache(dir, 6, u, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	request := func(name string) {
		p := testutil.Package(name, name)
		f, err := cache.Open(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil || string(data) != name {
			t.Fatalf("cached %q: %v", data, err)
		}
	}
	for _, name := range []string{"aaa", "bbb", "aaa", "ccc", "aaa"} {
		request(name)
	}
	if calls.Load() != 3 {
		t.Fatalf("got %d downloads", calls.Load())
	}
	cache, err = NewCache(dir, 6, u, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	request("aaa")
	request("bbb")
	if calls.Load() != 4 {
		t.Fatalf("expected one miss after restart, total %d", calls.Load())
	}
	request("oversize")
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("cache after oversized package: %v %v", files, err)
	}
	p := testutil.Package("bbb", "bad")
	if f, err := cache.Open(context.Background(), p); err == nil {
		f.Close()
		t.Fatal("accepted corrupt package")
	}
	if cache.Bytes != 6 {
		t.Fatalf("corrupt package changed cache: %d", cache.Bytes)
	}
}

func TestDiscardCacheFilePreservesReplacement(t *testing.T) {
	const body = "right"
	p := testutil.Package("test", body)
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, body)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	path := filepath.Join(dir, PackageCacheKey(p.Sum))
	if err := os.WriteFile(path, []byte("wrong"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewCache(dir, 100, u, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	bad, err := c.Stream(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	f := bad.CachedFile()
	if f == nil || VerifyPackage(bad, io.Discard, p) == nil {
		t.Fatal("expected a corrupt disk hit")
	}
	if err := c.DiscardFile(p, f); err != nil {
		t.Fatal(err)
	}
	read := func() {
		t.Helper()
		r, err := c.Stream(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil || string(data) != body {
			t.Fatalf("body %q, error %v", data, err)
		}
		if err := r.Verify(); err != nil {
			t.Fatal(err)
		}
	}
	read()
	// A slower request now reports the same failed descriptor.
	if err := c.DiscardFile(p, f); err != nil {
		t.Fatal(err)
	}
	read()
	if requests.Load() != 1 || c.Bytes != p.Size {
		t.Fatalf("late failure discarded new entry: requests %d, bytes %d", requests.Load(), c.Bytes)
	}
	if err := os.Truncate(path, 1); err != nil {
		t.Fatal(err)
	}
	read()
	if requests.Load() != 2 || c.Bytes != p.Size {
		t.Fatalf("truncated entry was not replaced: requests %d, bytes %d", requests.Load(), c.Bytes)
	}
}
