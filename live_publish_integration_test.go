package poudrine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto"
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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func putLiveFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func liveFixtureManifest(name, version string) string {
	return liveFixtureOriginManifest(name, "devel/"+name, version)
}

func liveFixtureOriginManifest(name, origin, version string) string {
	return fmt.Sprintf(`{"name":%q,"origin":%q,"version":%q,"abi":"FreeBSD:15:amd64","arch":"freebsd:15:x86:64","comment":"Test package","desc":"Test package","maintainer":"test@example.org","www":"https://example.org","prefix":"/usr/local","flatsize":0,"licenselogic":"single"}`, name, origin, version)
}

func liveFixtureArchive(t *testing.T, manifest string) []byte {
	t.Helper()
	var output bytes.Buffer
	tw := tar.NewWriter(&output)
	for _, member := range []string{"+COMPACT_MANIFEST", "+MANIFEST"} {
		if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0644, Size: int64(len(manifest))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func liveFixtureCatalogEntry(manifest, path string, data []byte) []byte {
	return fmt.Appendf(nil, `%s,"path":%q,"pkgsize":%d,"sum":"%x"}`, strings.TrimSuffix(manifest, "}"), path, len(data), sha256.Sum256(data))
}

func TestLivePackageInstallBeforeBulkCommit(t *testing.T) {
	for _, source := range []string{"upstream", "built", "fetched"} {
		t.Run(source, func(t *testing.T) { testLivePackageInstall(t, source) })
	}
}

func testLivePackageInstall(t *testing.T, dependencySource string) {
	ctx := t.Context()
	state := t.TempDir()
	cfg := DefaultBuilderConfig()
	cfg.BaseFS = t.TempDir()
	repoDir := cfg.RepositoryDir()
	started := time.Now().UTC().Add(-time.Minute)
	run := started.Format(buildNameFormat)
	logDir := filepath.Join(cfg.BaseFS, "data/logs/bulk", cfg.Jail+"-"+cfg.Ports, run)
	building := filepath.Join(repoDir, ".building")
	putLiveFixture(t, filepath.Join(building, ".buildname"), []byte(run+"\n"))
	const name, origin, version = "firefox", "www/firefox", "161.0,2"
	firefoxManifest := func(version, builtWithDav1d string) string {
		base := strings.TrimSuffix(liveFixtureOriginManifest(name, origin, version), "}")
		return fmt.Sprintf(`%s,"deps":{"dav1d":{"origin":"multimedia/dav1d","version":%q}},"shlibs_required":["libc.so.7","libdav1d.so.7"]}`, base, builtWithDav1d)
	}
	manifest := firefoxManifest(version, "1.5.5")
	archive := liveFixtureArchive(t, manifest)
	localPath := "All/" + name + "-" + version + ".pkg"
	putLiveFixture(t, filepath.Join(building, localPath), archive)
	for _, other := range []string{"unfinished", "failed"} {
		putLiveFixture(t, filepath.Join(building, "All", other+"-1.pkg"), liveFixtureArchive(t, liveFixtureManifest(other, "1")))
	}
	built := origin + " " + name + "-" + version + " 5\n"
	if dependencySource == "built" {
		built += "multimedia/dav1d dav1d-1.5.5 2\n"
	}
	putLiveFixture(t, filepath.Join(logDir, ".poudriere.ports.built"), []byte(built+"devel/unfinished unfinished-1 2"))
	putLiveFixture(t, filepath.Join(logDir, ".poudriere.ports.failed"), []byte("devel/failed failed-1 build error 5\n"))

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
	putLiveFixture(t, filepath.Join(trust, "trusted", "repo"), fmt.Appendf(nil, "function: \"sha256\"\nfingerprint: \"%x\"\n", sha256.Sum256(pub)))
	const dav1dPath = "All/dav1d-1.5.4_1.pkg"
	dav1dManifest := strings.TrimSuffix(liveFixtureOriginManifest("dav1d", "multimedia/dav1d", "1.5.4_1"), "}") + `,"shlibs_provided":["libdav1d.so.7"]}`
	dav1dArchive := liveFixtureArchive(t, dav1dManifest)
	wantDav1d := dav1dArchive
	wantVersion := "1.5.4_1"
	const newDepPath = "All/dav1d-1.5.5.pkg"
	var depEntry []byte
	if dependencySource != "upstream" {
		// The existing compiled package and verified FreeBSD catalog both
		// offer 1.5.4_1, as on the reported worker.
		putLiveFixture(t, filepath.Join(repoDir, ".real_old", dav1dPath), dav1dArchive)
		if err := os.Symlink(".real_old/All", filepath.Join(repoDir, "All")); err != nil {
			t.Fatal(err)
		}
		oldRow := liveFixtureCatalogEntry(dav1dManifest, dav1dPath, dav1dArchive)
		index, err := signedPkg(upKey, "data", fmt.Appendf(nil, `{"packages":[%s]}`, oldRow))
		if err != nil {
			t.Fatal(err)
		}
		putLiveFixture(t, filepath.Join(repoDir, "data.pkg"), index)
		wantVersion = "1.5.5"
		newManifest := strings.Replace(dav1dManifest, "1.5.4_1", wantVersion, 1)
		wantDav1d = liveFixtureArchive(t, newManifest)
		depEntry = liveFixtureCatalogEntry(newManifest, newDepPath, wantDav1d)
		putLiveFixture(t, filepath.Join(building, newDepPath), wantDav1d)
		if dependencySource == "fetched" {
			cacheDir := filepath.Join(repoDir, ".pkg-cache")
			if err := os.MkdirAll(cacheDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(filepath.Join(building, newDepPath), filepath.Join(cacheDir, filepath.Base(newDepPath))); err != nil {
				t.Fatal(err)
			}
			putLiveFixture(t, filepath.Join(logDir, ".poudriere.ports.fetched"), []byte("multimedia/dav1d dav1d-1.5.5\n"))
		}
	}
	upRows := bytes.Join([][]byte{
		liveFixtureCatalogEntry(firefoxManifest("160.0,2", "1.5.4_1"), "All/firefox-160.0,2.pkg", []byte("upstream")),
		liveFixtureCatalogEntry(dav1dManifest, dav1dPath, dav1dArchive),
	}, []byte("\n"))
	upArchive := upstreamFixture(t, upKey, "packagesite.yaml", upRows, pub, false)
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
	cat, err := NewCatalog(state, up.URL, trust, repoDir, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	cat.jailRoot = filepath.Join(cfg.BaseFS, "jails", cfg.Jail)
	putLiveFixture(t, filepath.Join(cat.jailRoot, "lib", "libc.so.7"), []byte("base system library"))
	if err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	before := upstreamRequests.Load()
	// The worker has restarted and its status describes no active job, but
	// Poudriere retained this named stage and the matching success list.
	cfg.StatusFile = filepath.Join(t.TempDir(), "build.json")
	status, err := json.Marshal(BuildStatus{Stage: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	putLiveFixture(t, cfg.StatusFile, status)
	monitorCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitorLivePackages(monitorCtx, options{builder: cfg, localRepo: repoDir}, cat)
	}()
	for cat.current.Load().compiled[name].Version != version && monitorCtx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-done
	if cat.current.Load().compiled[name].Version != version {
		t.Fatal("monitor did not publish the completed package from the retained native stage")
	}
	// A jail upgrade can supply a previously missing library while the
	// archives and upstream revision are unchanged.
	candidates := cat.live
	libc := filepath.Join(cat.jailRoot, "lib", "libc.so.7")
	if err := os.Remove(libc); err != nil {
		t.Fatal(err)
	}
	if !cat.publicationInputsChanged() {
		t.Fatal("missed a change in the target jail's libraries")
	}
	if err := cat.PublishLive(ctx, candidates); err != nil {
		t.Fatal(err)
	}
	if h := cat.current.Load().liveHolds[name+"-"+version]; !strings.Contains(h.reason, "libc.so.7") {
		t.Fatalf("missing jail library did not hold Firefox: %+v", h)
	}
	putLiveFixture(t, libc, []byte("base system library"))
	if err := cat.PublishLive(ctx, candidates); err != nil {
		t.Fatal(err)
	}
	if upstreamRequests.Load() != before {
		t.Fatal("publishing a local package contacted the upstream repository")
	}
	db, err := openInventory(state, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cache, err := newPackageCache(t.TempDir(), 1<<20, cat.upstream, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	clientRepo := httptest.NewServer(&repository{catalog: cat, cache: cache, localDir: repoDir, store: inventory{db}})
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
	files, err := unpackCatalog(bytes.NewReader(get("data.pkg")), "data")
	if err != nil {
		t.Fatal(err)
	}
	sig := files["signature"]
	if len(sig) != cat.key.Size()+1 || rsa.VerifyPKCS1v15(&cat.key.PublicKey, crypto.SHA256, pkgSignatureDigest(files["data"]), sig[:len(sig)-1]) != nil {
		t.Fatal("invalid live catalog signature")
	}
	content, err := parseCatalog(files["data"], "data", false)
	if err != nil {
		t.Fatal(err)
	}
	firefoxEntry := content.byName[name]
	selected := firefoxEntry.pkg
	if len(content.byName) != 2 || selected.Version != version || selected.Sum != fmt.Sprintf("%x", sha256.Sum256(archive)) {
		t.Fatalf("unexpected packages during the bulk: %+v", content.byName)
	}
	var firefoxMetadata struct {
		Deps map[string]struct{ Version string }
	}
	if err := json.Unmarshal(firefoxEntry.raw, &firefoxMetadata); err != nil || firefoxMetadata.Deps["dav1d"].Version != "1.5.5" {
		t.Fatalf("published Firefox manifest lost its build dependency version: %s", firefoxEntry.raw)
	}
	dep := content.byName["dav1d"].pkg
	if dep.Version != wantVersion || !bytes.Equal(get(dep.Path), wantDav1d) {
		t.Fatalf("wanted dav1d %s in signed catalog, got %+v", wantVersion, dep)
	}
	snapshot := cat.current.Load()
	switch dependencySource {
	case "built":
		if snapshot.compiled["dav1d"].Version != wantVersion || snapshot.ahead != 2 {
			t.Fatalf("newer compiled dependency missing from build counts")
		}
	case "fetched":
		if !snapshot.fetched["dav1d"] || snapshot.compiled["dav1d"].Version != "1.5.4_1" || snapshot.ahead != 1 {
			t.Fatalf("worker-fetched dependency counted as a local build")
		}
	}
	if !bytes.Equal(get(selected.Path), archive) {
		t.Fatal("live package contents differ")
	}
	t.Run("FreeBSD pkg install", func(t *testing.T) {
		if runtime.GOOS != "freebsd" {
			t.Skip("requires FreeBSD pkg")
		}
		const pkg = "/usr/local/sbin/pkg"
		if _, err := os.Stat(pkg); err != nil {
			t.Skip("pkg is not installed")
		}
		dir := t.TempDir()
		conf := filepath.Join(dir, "pkg.conf")
		putLiveFixture(t, conf, fmt.Appendf(nil, "REPOS_DIR: [%q]\nPKG_DBDIR: %q\nPKG_CACHEDIR: %q\nABI: FreeBSD:15:amd64\n", filepath.Join(dir, "repos"), filepath.Join(dir, "db"), filepath.Join(dir, "cache")))
		putLiveFixture(t, filepath.Join(dir, "repos", "Poudrine.conf"), fmt.Appendf(nil, "Poudrine: { url: %q, signature_type: pubkey, pubkey: %q }\n", clientRepo.URL, filepath.Join(state, "repository.pub")))
		cmd := exec.CommandContext(t.Context(), pkg, "-o", "INSTALL_AS_USER=yes", "-C", conf, "install", "--fetch-only", "-y", "-r", "Poudrine", name)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pkg install --fetch-only: %v\n%s", err, out)
		}
		var foundFirefox, foundDav1d bool
		err := filepath.WalkDir(filepath.Join(dir, "cache"), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.Type().IsRegular() || filepath.Ext(path) != ".pkg" {
				return err
			}
			body, err := os.ReadFile(path)
			foundFirefox = foundFirefox || bytes.Equal(body, archive)
			foundDav1d = foundDav1d || bytes.Equal(body, wantDav1d)
			return err
		})
		if err != nil || !foundFirefox || !foundDav1d {
			t.Fatalf("pkg install --fetch-only cache: Firefox %t, dav1d %t, error %v", foundFirefox, foundDav1d, err)
		}
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
	finalIndex, err := signedPkg(upKey, "data", fmt.Appendf(nil, `{"packages":[%s]}`, entry))
	if err != nil {
		t.Fatal(err)
	}
	putLiveFixture(t, filepath.Join(repoDir, "data.pkg"), finalIndex)
	if !cat.publicationInputsChanged() {
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
	if cat.publicationInputsChanged() || upstreamRequests.Load() != before {
		t.Fatal("local commit did not reconcile independently of upstream")
	}

	later := started.Add(30 * time.Second)
	nextRun := later.Format(buildNameFormat)
	const laterVersion = "161.0_1,2"
	uncommitted := liveFixtureArchive(t, firefoxManifest(laterVersion, "1.5.5"))
	putLiveFixture(t, filepath.Join(building, ".buildname"), []byte(nextRun+"\n"))
	putLiveFixture(t, filepath.Join(building, "All", name+"-"+laterVersion+".pkg"), uncommitted)
	putLiveFixture(t, filepath.Join(filepath.Dir(logDir), nextRun, ".poudriere.ports.built"), []byte(origin+" "+name+"-"+laterVersion+" 5\n"))
	staging := liveArchiveStore{dir: cat.liveDir, repoDir: repoDir}
	live, err := staging.scan(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.PublishLive(ctx, live); err != nil {
		t.Fatal(err)
	}
	pkg := cat.current.Load().compiled[name]
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
	if cat.current.Load().compiled[name].Version != version || !bytes.Equal(get(pkg.Path), uncommitted) {
		t.Fatal("rollback lost the committed package or invalidated an advertised URL")
	}
}
