package catalog

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func upstreamSnapshot(packages ...packageinfo.Package) *Snapshot {
	s := &Snapshot{ByPath: map[string]packageinfo.Package{}, PathByName: map[string]string{}}
	for _, p := range packages {
		s.ByPath[p.Path] = p
		s.PathByName[p.Name] = p.Path
	}
	return s
}

func checkCachedRebuilds(t *testing.T, c *Cache, s *Snapshot, want ...packageinfo.Package) {
	t.Helper()
	var wantKeys, files, rows []string
	var wantBytes int64
	for _, p := range want {
		wantKeys = append(wantKeys, PackageCacheKey(p.Sum))
		wantBytes += p.Size
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		files = append(files, entry.Name())
	}
	view := ManagementPackages(s, nil, nil, c.Inventory(s), nil, nil)
	for _, row := range view.Packages {
		rows = append(rows, row.CacheKey)
	}
	slices.Sort(wantKeys)
	slices.Sort(rows)
	if !slices.Equal(files, wantKeys) || !slices.Equal(rows, wantKeys) ||
		c.Bytes != wantBytes || view.CachedBytes != wantBytes || view.CachedCount != len(want) {
		t.Fatalf("disk %v, inventory %v, cache bytes %d, inventory totals %d/%d bytes; want %v, %d bytes",
			files, rows, c.Bytes, view.CachedCount, view.CachedBytes, wantKeys, wantBytes)
	}
}

func TestCacheRefreshFollowsUpstreamChecksum(t *testing.T) {
	const first, rebuilt = "first archive", "fresh archive"
	old := testutil.Package("wheel", first)
	current := testutil.Package("wheel", rebuilt)
	var body atomic.Value
	body.Store(first)
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, body.Load().(string))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	cache, err := NewCache(t.TempDir(), 1<<20, u, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	get := func(p packageinfo.Package, want string) {
		t.Helper()
		f, err := cache.Open(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil || string(data) != want {
			t.Fatalf("package bytes = %q, want %q: %v", data, want, err)
		}
	}
	if err := cache.Prune(upstreamSnapshot(old)); err != nil {
		t.Fatal(err)
	}
	get(old, first)
	if err := cache.Prune(upstreamSnapshot(old)); err != nil {
		t.Fatal(err)
	}
	get(old, first)
	if n := requests.Load(); n != 1 {
		t.Fatalf("unchanged checksum made %d requests, want 1", n)
	}
	body.Store(rebuilt)
	s := upstreamSnapshot(current)
	if err := cache.Prune(s); err != nil {
		t.Fatal(err)
	}
	checkCachedRebuilds(t, cache, s)
	if n := requests.Load(); n != 1 {
		t.Fatalf("catalog refresh fetched replacement: %d requests", n)
	}
	get(current, rebuilt)
	checkCachedRebuilds(t, cache, s, current)
	if n := requests.Load(); n != 2 {
		t.Fatalf("first client request for replacement: %d requests, want 2", n)
	}
	// A caller can still hold older signed metadata after the switch.
	body.Store(first)
	get(old, first)
	checkCachedRebuilds(t, cache, s, current)
}

func TestCacheRefreshPrunesRestoredChecksumKeys(t *testing.T) {
	dir := t.TempDir()
	keep := testutil.Package("current", "signed body")
	rebuilt := testutil.Package("current", "older bytes")
	removed := testutil.Package("removed", "unlisted")
	old := testutil.Package("versioned", "old version")
	next := testutil.Package("versioned", "next version")
	next.Version = "2"
	// All entries lack a readable manifest. The current one even has a
	// same-size content mismatch: refresh uses only the signed checksum key.
	for p, body := range map[packageinfo.Package]string{keep: "broken body", rebuilt: "older bytes", removed: "unlisted", old: "old version"} {
		testutil.WriteFile(t, filepath.Join(dir, PackageCacheKey(p.Sum)), []byte(body))
	}
	cache, err := NewCache(dir, 1<<20, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := upstreamSnapshot(keep, next)
	if err := cache.Prune(s); err != nil {
		t.Fatal(err)
	}
	checkCachedRebuilds(t, cache, s, keep)
}

func TestCacheLateDownloadAfterUpstreamRefresh(t *testing.T) {
	const first = "first build"
	old := testutil.Package("wheel", first)
	current := testutil.Package("wheel", "later build")
	finish := make(chan struct{})
	var release sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, first[:4])
		w.(http.Flusher).Flush()
		<-finish
		io.WriteString(w, first[4:])
	}))
	defer up.Close()
	defer release.Do(func() { close(finish) })
	u, _ := url.Parse(up.URL)
	cache, err := NewCache(t.TempDir(), 1<<20, u, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Prune(upstreamSnapshot(old)); err != nil {
		t.Fatal(err)
	}
	reader, err := cache.Stream(t.Context(), old)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	s := upstreamSnapshot(current)
	if err := cache.Prune(s); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(finish) })
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != first {
		t.Fatalf("existing reader received %q, %v", body, err)
	}
	if err := reader.Verify(); err != nil {
		t.Fatal(err)
	}
	reader.Close()
	checkCachedRebuilds(t, cache, s)
}
