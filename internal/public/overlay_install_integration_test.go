package public

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/testutil"
)

// The local port and the official package have the same name and version.
// Exercise pkg's dependency solver and archive downloads through the repo.
func TestPkgFetchMyDumperOverlay(t *testing.T) {
	const origin = "databases/mydumper"
	const path = "All/mydumper-1.pkg"
	const mariaName, mariaVersion = "mariadb123-client", "12.3.3"
	const mariaPath = "All/" + mariaName + "-" + mariaVersion + ".pkg"
	base := strings.TrimSuffix(testutil.OriginManifest("mydumper", origin, "1"), "}")
	clientDep := func(name, version string) string {
		return fmt.Sprintf(`,"deps":{%q:{"origin":%q,"version":%q}}`, name, "databases/"+name, version)
	}
	maria := testutil.OriginManifest(mariaName, "databases/"+mariaName, mariaVersion)
	mariaArchive := testutil.Archive(t, maria)
	stock := base + clientDep("mysql84-client", "8.4") + "}"
	stockArchive := testutil.Archive(t, stock)
	overlays := config.PackageOverlays{origin: {DefaultVersions: map[string]string{"mysql": "12.3m"}}}
	custom := base + clientDep(mariaName, mariaVersion) + fmt.Sprintf(`,"annotations":{%q:%q}}`, config.OverlayAnnotation, overlays[origin].Digest())
	customArchive := testutil.Archive(t, custom)
	key, public, trust := liveFixtureTrust(t)
	stockCatalog := testutil.UpstreamFixture(t, catalog.PkgSignatureDigest, key, "packagesite.yaml", bytes.Join([][]byte{
		liveFixtureCatalogEntry(stock, path, stockArchive),
		liveFixtureCatalogEntry(maria, mariaPath, mariaArchive),
	}, []byte("\n")), public, false)
	var stockRequests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/packagesite.pkg":
			w.Write(stockCatalog)
		case "/" + path:
			stockRequests.Add(1)
			w.Write(stockArchive)
		case "/" + mariaPath:
			w.Write(mariaArchive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	state, local := t.TempDir(), t.TempDir()
	cat, err := catalog.New(state, up.URL, trust, local, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	cat.Overlays = overlays
	if err := cat.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	cache, err := catalog.NewCache(t.TempDir(), 1<<20, cat.Upstream, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(state, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := httptest.NewServer(&Repository{Catalog: cat, Cache: cache, LocalDir: local, Store: (*db)})
	defer repo.Close()
	res, err := repo.Client().Get(repo.URL + "/" + path)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("before custom build, stock download returned %s", res.Status)
	}
	index, err := catalog.SignedPkg(key, "packagesite.yaml", liveFixtureCatalogEntry(custom, path, customArchive))
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(local, path), customArchive)
	testutil.WriteFile(t, filepath.Join(local, "packagesite.pkg"), index)
	if err := cat.PublishLive(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	t.Run("FreeBSD pkg install", func(t *testing.T) {
		checkPkgFetch(t, repo.URL, filepath.Join(state, "repository.pub"), "mydumper", customArchive, mariaArchive)
	})
	if n := stockRequests.Load(); n != 0 {
		t.Errorf("proxied the official mydumper archive %d times", n)
	}
}
