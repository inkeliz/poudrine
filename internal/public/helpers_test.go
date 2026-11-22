package public

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func liveFixtureCatalogEntry(manifest, path string, data []byte) []byte {
	return fmt.Appendf(nil, `%s,"path":%q,"pkgsize":%d,"sum":"%x"}`, strings.TrimSuffix(manifest, "}"), path, len(data), sha256.Sum256(data))
}

func liveFixtureTrust(t *testing.T) (*rsa.PrivateKey, []byte, string) {
	t.Helper()
	upKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&upKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	trust := t.TempDir()
	testutil.WriteFile(t, filepath.Join(trust, "trusted", "repo"), fmt.Appendf(nil, "function: \"sha256\"\nfingerprint: \"%x\"\n", sha256.Sum256(pub)))
	return upKey, pub, trust
}

// Run pkg with an isolated catalog and cache. Matching the archive bytes also
// distinguishes two variants with the same name and version.
func checkPkgFetch(t *testing.T, repoURL, pubKeyPath, pkgName string, archives ...[]byte) {
	t.Helper()
	const pkg = "/usr/local/sbin/pkg"
	if _, err := os.Stat(pkg); err != nil {
		t.Skip("pkg is not installed")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "pkg.conf")
	testutil.WriteFile(t, conf, fmt.Appendf(nil, "REPOS_DIR: [%q]\nPKG_DBDIR: %q\nPKG_CACHEDIR: %q\nABI: FreeBSD:15:amd64\n", filepath.Join(dir, "repos"), filepath.Join(dir, "db"), filepath.Join(dir, "cache")))
	testutil.WriteFile(t, filepath.Join(dir, "repos", "Poudrine.conf"), fmt.Appendf(nil, "Poudrine: { url: %q, signature_type: pubkey, pubkey: %q }\n", repoURL, pubKeyPath))
	cmd := exec.CommandContext(t.Context(), pkg, "-o", "INSTALL_AS_USER=yes", "-C", conf, "install", "--fetch-only", "-y", "-r", "Poudrine", pkgName)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pkg install --fetch-only: %v\n%s", err, out)
	}
	found := make([]bool, len(archives))
	err := filepath.WalkDir(filepath.Join(dir, "cache"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() || filepath.Ext(path) != ".pkg" {
			return err
		}
		body, err := os.ReadFile(path)
		for i, archive := range archives {
			if bytes.Equal(archive, body) {
				found[i] = true
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, ok := range found {
		if !ok {
			t.Errorf("pkg install --fetch-only %s did not fetch archive %d", pkgName, i+1)
		}
	}
}

func testLivePackageInstall(t *testing.T, dependencySource string) {
	ctx := t.Context()
	state := t.TempDir()
	cfg := config.DefaultBuilder()
	cfg.BaseFS = t.TempDir()
	repoDir := cfg.RepositoryDir()
	started := time.Now().UTC().Add(-time.Minute)
	run := started.Format(builder.BuildNameFormat)
	logDir := filepath.Join(cfg.BaseFS, "data/logs/bulk", cfg.Jail+"-"+cfg.Ports, run)
	building := filepath.Join(repoDir, ".building")
	testutil.WriteFile(t, filepath.Join(building, ".buildname"), []byte(run+"\n"))
	const name, origin, version = "firefox", "www/firefox", "161.0,2"
	firefoxManifest := func(version, builtWithDav1d string) string {
		base := strings.TrimSuffix(testutil.OriginManifest(name, origin, version), "}")
		return fmt.Sprintf(`%s,"deps":{"dav1d":{"origin":"multimedia/dav1d","version":%q}},"shlibs_required":["libc.so.7","libdav1d.so.7"]}`, base, builtWithDav1d)
	}
	manifest := firefoxManifest(version, "1.5.5")
	archive := testutil.Archive(t, manifest)
	localPath := "All/" + name + "-" + version + ".pkg"
	testutil.WriteFile(t, filepath.Join(building, localPath), archive)
	for _, other := range []string{"unfinished", "failed"} {
		testutil.WriteFile(t, filepath.Join(building, "All", other+"-1.pkg"), testutil.Archive(t, testutil.Manifest(other, "1")))
	}
	built := origin + " " + name + "-" + version + " 5\n"
	if dependencySource == "built" {
		built += "multimedia/dav1d dav1d-1.5.5 2\n"
	}
	testutil.WriteFile(t, filepath.Join(logDir, ".poudriere.ports.built"), []byte(built+"devel/unfinished unfinished-1 2"))
	testutil.WriteFile(t, filepath.Join(logDir, ".poudriere.ports.failed"), []byte("devel/failed failed-1 build error 5\n"))

	upKey, pub, trust := liveFixtureTrust(t)
	const dav1dPath = "All/dav1d-1.5.4_1.pkg"
	dav1dManifest := strings.TrimSuffix(testutil.OriginManifest("dav1d", "multimedia/dav1d", "1.5.4_1"), "}") + `,"shlibs_provided":["libdav1d.so.7"]}`
	dav1dArchive := testutil.Archive(t, dav1dManifest)
	wantDav1d := dav1dArchive
	wantVersion := "1.5.4_1"
	const newDepPath = "All/dav1d-1.5.5.pkg"
	var depEntry []byte
	if dependencySource != "upstream" {
		// The existing compiled package and verified FreeBSD catalog both
		// offer 1.5.4_1, as on the reported worker.
		testutil.WriteFile(t, filepath.Join(repoDir, ".real_old", dav1dPath), dav1dArchive)
		if err := os.Symlink(".real_old/All", filepath.Join(repoDir, "All")); err != nil {
			t.Fatal(err)
		}
		oldRow := liveFixtureCatalogEntry(dav1dManifest, dav1dPath, dav1dArchive)
		index, err := catalog.SignedPkg(upKey, "data", fmt.Appendf(nil, `{"packages":[%s]}`, oldRow))
		if err != nil {
			t.Fatal(err)
		}
		testutil.WriteFile(t, filepath.Join(repoDir, "data.pkg"), index)
		wantVersion = "1.5.5"
		newManifest := strings.Replace(dav1dManifest, "1.5.4_1", wantVersion, 1)
		wantDav1d = testutil.Archive(t, newManifest)
		depEntry = liveFixtureCatalogEntry(newManifest, newDepPath, wantDav1d)
		testutil.WriteFile(t, filepath.Join(building, newDepPath), wantDav1d)
		if dependencySource == "fetched" {
			cacheDir := filepath.Join(repoDir, ".pkg-cache")
			if err := os.MkdirAll(cacheDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(filepath.Join(building, newDepPath), filepath.Join(cacheDir, filepath.Base(newDepPath))); err != nil {
				t.Fatal(err)
			}
			testutil.WriteFile(t, filepath.Join(logDir, ".poudriere.ports.fetched"), []byte("multimedia/dav1d dav1d-1.5.5\n"))
		}
	}
	upRows := bytes.Join([][]byte{
		liveFixtureCatalogEntry(firefoxManifest("160.0,2", "1.5.4_1"), "All/firefox-160.0,2.pkg", []byte("upstream")),
		liveFixtureCatalogEntry(dav1dManifest, dav1dPath, dav1dArchive),
	}, []byte("\n"))
	upArchive := testutil.UpstreamFixture(t, catalog.PkgSignatureDigest, upKey, "packagesite.yaml", upRows, pub, false)
	var upstreamRequests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		switch r.URL.Path {
		case "/packagesite.pkg":
			w.Write(upArchive)
		case "/" + dav1dPath:
			w.Write(dav1dArchive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	cat, err := catalog.New(state, up.URL, trust, repoDir, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	cat.JailRoot = filepath.Join(cfg.BaseFS, "jails", cfg.Jail)
	testutil.WriteFile(t, filepath.Join(cat.JailRoot, "lib", "libc.so.7"), []byte("base system library"))
	if err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	before := upstreamRequests.Load()
	// The worker has restarted and its status describes no active job, but
	// Poudriere retained this named stage and the matching success list.
	cfg.StatusFile = filepath.Join(t.TempDir(), "build.json")
	status, err := json.Marshal(builder.BuildStatus{Stage: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, cfg.StatusFile, status)
	monitorCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		catalog.MonitorLivePackages(monitorCtx, config.Options{Builder: cfg, LocalRepo: repoDir}, cat)
	}()
	waitErr := testutil.Wait(monitorCtx, func() bool { return cat.Current.Load().Compiled[name].Version == version })
	stop()
	<-done
	if waitErr != nil {
		t.Fatalf("waiting for live package publication: %v", waitErr)
	}
	// A jail upgrade can supply a previously missing library while the
	// archives and upstream revision are unchanged.
	candidates := cat.Live
	libc := filepath.Join(cat.JailRoot, "lib", "libc.so.7")
	if err := os.Remove(libc); err != nil {
		t.Fatal(err)
	}
	if !cat.PublicationInputsChanged() {
		t.Fatal("missed a change in the target jail's libraries")
	}
	if err := cat.PublishLive(ctx, candidates); err != nil {
		t.Fatal(err)
	}
	if h := cat.Current.Load().LiveHolds[name+"-"+version]; !strings.Contains(h.Reason, "libc.so.7") {
		t.Fatalf("missing jail library did not hold Firefox: %+v", h)
	}
	testutil.WriteFile(t, libc, []byte("base system library"))
	if err := cat.PublishLive(ctx, candidates); err != nil {
		t.Fatal(err)
	}
	if upstreamRequests.Load() != before {
		t.Fatal("publishing a local package contacted the upstream repository")
	}
	db, err := database.Open(state, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cache, err := catalog.NewCache(t.TempDir(), 1<<20, cat.Upstream, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	clientRepo := httptest.NewServer(&Repository{Catalog: cat, Cache: cache, LocalDir: repoDir, Store: (*db)})
	defer clientRepo.Close()
	get := func(path string) []byte {
		t.Helper()
		resp, err := clientRepo.Client().Get(clientRepo.URL + "/" + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %s", path, resp.Status)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	// A client follows the path from the signed metadata, without knowing
	// whether Poudriere has committed its repository yet.
	files, err := catalog.UnpackCatalog(bytes.NewReader(get("data.pkg")), "data")
	if err != nil {
		t.Fatal(err)
	}
	sig := files["signature"]
	if len(sig) != cat.Key.Size()+1 || rsa.VerifyPKCS1v15(&cat.Key.PublicKey, crypto.SHA256, catalog.PkgSignatureDigest(files["data"]), sig[:len(sig)-1]) != nil {
		t.Fatal("invalid live catalog signature")
	}
	content, err := catalog.ParseCatalog(files["data"], "data", false)
	if err != nil {
		t.Fatal(err)
	}
	firefoxEntry := content.ByName[name]
	selected := firefoxEntry.Pkg
	if len(content.ByName) != 2 || selected.Version != version || selected.Sum != fmt.Sprintf("%x", sha256.Sum256(archive)) {
		t.Fatalf("unexpected packages during the bulk: %+v", content.ByName)
	}
	var firefoxMetadata struct {
		Deps map[string]struct{ Version string }
	}
	if err := json.Unmarshal(firefoxEntry.Raw, &firefoxMetadata); err != nil || firefoxMetadata.Deps["dav1d"].Version != "1.5.5" {
		t.Fatalf("published Firefox manifest lost its build dependency version: %s", firefoxEntry.Raw)
	}
	dep := content.ByName["dav1d"].Pkg
	if dep.Version != wantVersion || !bytes.Equal(get(dep.Path), wantDav1d) {
		t.Fatalf("wanted dav1d %s in signed catalog, got %+v", wantVersion, dep)
	}
	snapshot := cat.Current.Load()
	switch dependencySource {
	case "built":
		if snapshot.Compiled["dav1d"].Version != wantVersion || snapshot.Ahead != 2 {
			t.Fatalf("newer compiled dependency missing from build counts")
		}
	case "fetched":
		if !snapshot.Fetched["dav1d"] || snapshot.Compiled["dav1d"].Version != "1.5.4_1" || snapshot.Ahead != 1 {
			t.Fatalf("worker-fetched dependency counted as a local build")
		}
	}
	if !bytes.Equal(get(selected.Path), archive) {
		t.Fatal("live package contents differ")
	}
	t.Run("FreeBSD pkg install", func(t *testing.T) {
		checkPkgFetch(t, clientRepo.URL, filepath.Join(state, "repository.pub"), name, archive, wantDav1d)
	})
	before = upstreamRequests.Load()

	// Poudriere's commit replaces .building. Previously advertised paths
	// must still work for clients who refreshed their pkg catalog earlier.
	if err := os.Rename(building, filepath.Join(repoDir, ".real_"+run)); err != nil {
		t.Fatal(err)
	}
	if dependencySource != "upstream" {
		if err := os.Remove(filepath.Join(repoDir, "All")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(".real_"+run, "All"), filepath.Join(repoDir, "All")); err != nil {
		t.Fatal(err)
	}
	// A native data-only commit must publish from the verified baseline even
	// if the upstream mirror is unavailable at that moment.
	entry := liveFixtureCatalogEntry(manifest, localPath, archive)
	if len(depEntry) > 0 {
		entry = bytes.Join([][]byte{entry, depEntry}, []byte(","))
	}
	finalIndex, err := catalog.SignedPkg(upKey, "data", fmt.Appendf(nil, `{"packages":[%s]}`, entry))
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(repoDir, "data.pkg"), finalIndex)
	if !cat.PublicationInputsChanged() {
		t.Fatal("missed the committed native data.pkg")
	}
	if err := cat.PublishLive(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(get(localPath), archive) || !bytes.Equal(get(selected.Path), archive) {
		t.Fatal("a package download changed across the bulk commit")
	}
	if depEntry != nil && (!bytes.Equal(get(newDepPath), wantDav1d) || !bytes.Equal(get(dep.Path), wantDav1d)) {
		t.Fatal("the committed or previously advertised dependency is unavailable")
	}
	if cat.PublicationInputsChanged() || upstreamRequests.Load() != before {
		t.Fatal("local commit did not reconcile independently of upstream")
	}

	later := started.Add(30 * time.Second)
	nextRun := later.Format(builder.BuildNameFormat)
	const laterVersion = "161.0_1,2"
	uncommitted := testutil.Archive(t, firefoxManifest(laterVersion, "1.5.5"))
	testutil.WriteFile(t, filepath.Join(building, ".buildname"), []byte(nextRun+"\n"))
	testutil.WriteFile(t, filepath.Join(building, "All", name+"-"+laterVersion+".pkg"), uncommitted)
	testutil.WriteFile(t, filepath.Join(filepath.Dir(logDir), nextRun, ".poudriere.ports.built"), []byte(origin+" "+name+"-"+laterVersion+" 5\n"))
	staging := catalog.LiveArchiveStore{Dir: cat.LiveDir, RepoDir: repoDir}
	live, err := staging.Scan(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.PublishLive(ctx, live); err != nil {
		t.Fatal(err)
	}
	pkg := cat.Current.Load().Compiled[name]
	if pkg.Version != laterVersion {
		t.Fatalf("second build did not publish: %+v", pkg)
	}
	// If Poudriere discards the next bulk, revert to its last committed
	// package. Clients that saw the newer metadata can finish their download.
	if err := os.RemoveAll(building); err != nil {
		t.Fatal(err)
	}
	if err := cat.PublishLive(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if cat.Current.Load().Compiled[name].Version != version || !bytes.Equal(get(pkg.Path), uncommitted) {
		t.Fatal("rollback lost the committed package or invalidated an advertised URL")
	}
}

func inventoryReportFixture(t *testing.T) (*Repository, packageinfo.Package) {
	t.Helper()
	db, err := database.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	pkg := packageinfo.Package{Name: "py312-wheel", Version: "5", Origin: "devel/py-wheel", BuildOrigin: "devel/py-wheel@py312", Path: "All/wheel-5.pkg"}
	base := packageinfo.Package{Name: "FreeBSD-acct", Origin: "base/FreeBSD-acct", BuildOrigin: "base/FreeBSD-acct", Path: "All/acct.pkg"}
	cat := &catalog.Catalog{}
	cat.Current.Store(&catalog.Snapshot{
		ByPath:     map[string]packageinfo.Package{pkg.Path: pkg, base.Path: base},
		PathByName: map[string]string{pkg.Name: pkg.Path, base.Name: base.Path},
	})
	// The fixture intentionally has no package cache or upstream connection.
	return &Repository{Catalog: cat, Store: (*db)}, pkg
}

func newSignedReport(t *testing.T, key ed25519.PrivateKey, path, body string, at time.Time) *http.Request {
	t.Helper()
	r := testutil.SignedRequest(t, key, http.MethodPost, path, []byte(body), at)
	r.Header.Set("Content-Type", "text/plain; charset=utf-8")
	return r
}

func inventoryPost(t *testing.T, repo *Repository, body string, want int) inventoryReceipt {
	t.Helper()
	r := newSignedReport(t, testutil.ReportKey(), routes.ReportInventory.Path, body, time.Now())
	w := httptest.NewRecorder()
	repo.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("report returned %d, want %d: %s", w.Code, want, w.Body.String())
	}
	var receipt inventoryReceipt
	if want == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
	}
	return receipt
}

type metadataClientClosing struct {
	*httptest.ResponseRecorder
	closeClient context.CancelFunc
}

func (w metadataClientClosing) WriteHeader(code int) {
	w.ResponseRecorder.WriteHeader(code)
	w.closeClient()
}
