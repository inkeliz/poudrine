package public

import (
	"context"
	"fmt"
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

	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

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
	cache, err := catalog.NewCache(filepath.Join(dir, "cache"), 128, u, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := *db
	p := testutil.Package("wheel", "fixture payload")
	p.BuildOrigin = "devel/wheel@py312"
	cat := &catalog.Catalog{}
	local := testutil.Package("local", "local bytes")
	local.Local = true
	if err := os.MkdirAll(filepath.Join(dir, "All"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, local.Path), []byte("local bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	cat.Current.Store(&catalog.Snapshot{ByPath: map[string]packageinfo.Package{p.Path: p, local.Path: local}})
	_, proxy, _ := net.ParseCIDR("127.0.0.1/32")
	repo := &Repository{Catalog: cat, Store: s, Cache: cache, LocalDir: dir, Proxy: proxy}
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
	if origins, err := s.Origins(ctx); err != nil || strings.Join(origins, ",") != p.BuildOrigin {
		t.Fatalf("origins %v, %v", origins, err)
	}
	clients, err := s.Clients(ctx)
	if err != nil || len(clients) != 8 {
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
	for _, path := range []string{"/setup-token", "/../inventory.sqlite", "/unlisted.pkg", routes.Clients.Path} {
		w := httptest.NewRecorder()
		repo.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}

func TestRepositoryCountsMetadataClientAfterDisconnect(t *testing.T) {
	db, err := database.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := *db
	c := &catalog.Catalog{}
	c.Current.Store(&catalog.Snapshot{Metadata: map[string]catalog.MetadataFile{"meta.conf": {Body: []byte("version = 2;")}}})
	repo := Repository{Catalog: c, Store: s}

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
	clients, err := s.Clients(t.Context())
	if err != nil || len(clients) != 1 || len(clients[0].Packages) != 0 {
		t.Fatalf("metadata-only client: %v, %v", clients, err)
	}
}
