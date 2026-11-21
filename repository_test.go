package poudrine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func testPackage(name, body string) CatalogPackage {
	return CatalogPackage{Name: name, Version: "1", Path: "All/" + name + ".pkg", Origin: "devel/" + name, Sum: fmt.Sprintf("%x", sha256.Sum256([]byte(body))), Size: int64(len(body))}
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
	cache, err := newPackageCache(dir, 6, u, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	request := func(name string) {
		p := testPackage(name, name)
		f, err := cache.open(context.Background(), p)
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
	cache, err = newPackageCache(dir, 6, u, up.Client())
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
	p := testPackage("bbb", "bad")
	if f, err := cache.open(context.Background(), p); err == nil {
		f.Close()
		t.Fatal("accepted corrupt package")
	}
	if cache.bytes != 6 {
		t.Fatalf("corrupt package changed cache: %d", cache.bytes)
	}
}

func TestRepositoryInventoryAndConcurrentMiss(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, "fixture payload")
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	cache, err := newPackageCache(filepath.Join(dir, "cache"), 128, u, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	db, err := openInventory(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := inventory{db}
	p := testPackage("wheel", "fixture payload")
	p.BuildOrigin = "devel/wheel@py312"
	cat := &Catalog{}
	local := testPackage("local", "local bytes")
	local.Local = true
	if err := os.MkdirAll(filepath.Join(dir, "All"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, local.Path), []byte("local bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	cat.current.Store(&catalogSnapshot{byPath: map[string]CatalogPackage{p.Path: p, local.Path: local}})
	_, proxy, _ := net.ParseCIDR("127.0.0.1/32")
	repo := &repository{catalog: cat, store: s, cache: cache, localDir: dir, proxy: proxy}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			r := httptest.NewRequest("GET", "/"+p.Path, nil)
			r.RemoteAddr = "127.0.0.1:12345"
			r.Header.Set("X-Real-IP", fmt.Sprintf("192.0.2.%d", i+1))
			w := httptest.NewRecorder()
			repo.ServeHTTP(w, r)
			if w.Code != 200 || w.Body.String() != "fixture payload" {
				t.Errorf("response: %d %s", w.Code, w.Body)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("downloaded %d times", calls.Load())
	}
	if origins, err := s.origins(ctx); err != nil || strings.Join(origins, ",") != p.BuildOrigin {
		t.Fatalf("origins %v, %v", origins, err)
	}
	clients, err := s.clients(ctx)
	if err != nil || len(clients.([]clientRecord)) != 8 {
		t.Fatalf("client inventory %v, %v", clients, err)
	}
	r := httptest.NewRequest("GET", "/"+local.Path, nil)
	r.Header.Set("X-Real-IP", "192.0.2.199")
	r.Header.Set("Range", "bytes=0-4")
	w := httptest.NewRecorder()
	repo.ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "local" {
		t.Fatalf("local range: %d %q", w.Code, w.Body)
	}
	if repo.clientIP(r) == "192.0.2.199" {
		t.Fatal("trusted forwarded identity from unknown peer")
	}
	for _, path := range []string{"/setup-token", "/../inventory.sqlite", "/unlisted.pkg", "/api/admin/clients"} {
		w := httptest.NewRecorder()
		repo.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}

type metadataClientClosing struct {
	*httptest.ResponseRecorder
	closeClient context.CancelFunc
}

func (w metadataClientClosing) WriteHeader(code int) {
	w.ResponseRecorder.WriteHeader(code)
	w.closeClient()
}

func TestRepositoryCountsMetadataClientAfterDisconnect(t *testing.T) {
	db, err := openInventory(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := inventory{db}
	c := &Catalog{}
	c.current.Store(&catalogSnapshot{metadata: map[string]metadataFile{"meta.conf": {body: []byte("version = 2;")}}})
	repo := repository{catalog: c, store: s}

	// A pkg client can close its connection as soon as the catalog headers
	// arrive. The handler still owes the inventory a record of that visit.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequest("HEAD", "/meta.conf", nil).WithContext(ctx)
	w := metadataClientClosing{httptest.NewRecorder(), cancel}
	repo.ServeHTTP(w, req)
	if w.Code != http.StatusOK || ctx.Err() == nil {
		t.Fatalf("metadata response %d, client context %v", w.Code, ctx.Err())
	}
	clients, err := s.clients(t.Context())
	if err != nil || len(clients.([]clientRecord)) != 1 || len(clients.([]clientRecord)[0].Packages) != 0 {
		t.Fatalf("metadata-only client: %v, %v", clients, err)
	}
}
