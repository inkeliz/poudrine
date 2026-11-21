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
	return fmt.Sprintf(`{"name":%q,"origin":%q,"version":%q,"abi":"FreeBSD:15:amd64","arch":"freebsd:15:x86:64","comment":"Test package","desc":"Test package","maintainer":"test@example.org","www":"https://example.org","prefix":"/usr/local","flatsize":0,"licenselogic":"single"}`, name, "devel/"+name, version)
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
	const name = "publish-fixture"
	manifest := liveFixtureManifest(name, "2")
	archive := liveFixtureArchive(t, manifest)
	localPath := "All/" + name + "-2.pkg"
	putLiveFixture(t, filepath.Join(building, localPath), archive)
	for _, other := range []string{"unfinished", "failed"} {
		putLiveFixture(t, filepath.Join(building, "All", other+"-1.pkg"), liveFixtureArchive(t, liveFixtureManifest(other, "1")))
	}
	putLiveFixture(t, filepath.Join(logDir, ".poudriere.ports.built"), []byte("devel/"+name+" "+name+"-2 5\ndevel/unfinished unfinished-1 2"))
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
	upRow := liveFixtureCatalogEntry(liveFixtureManifest(name, "1"), "All/"+name+"-1.pkg", []byte("upstream"))
	upArchive := upstreamFixture(t, upKey, "packagesite.yaml", upRow, pub, false)
	var upstreamRequests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		if r.URL.Path != "/packagesite.pkg" {
			http.NotFound(w, r)
			return
		}
		w.Write(upArchive)
	}))
	defer up.Close()
	cat, err := NewCatalog(state, up.URL, trust, repoDir, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	before := upstreamRequests.Load()
	// Start the real monitor mid-bulk, just as a new web process would
	// after a service restart. It must discover previous successful jobs.
	cfg.StatusFile = filepath.Join(t.TempDir(), "build.json")
	status, err := json.Marshal(BuildStatus{Active: true, Stage: "building", Started: started})
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
	for cat.current.Load().compiled[name].Version != "2" && monitorCtx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-done
	if cat.current.Load().compiled[name].Version != "2" {
		t.Fatal("monitor did not publish the completed package from an active bulk")
	}
	if upstreamRequests.Load() != before {
		t.Fatal("publishing a local package contacted the upstream repository")
	}
	db, err := openInventory(state, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clientRepo := httptest.NewServer(&repository{catalog: cat, localDir: repoDir, store: inventory{db}})
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
	selected := content.byName[name].pkg
	if len(content.byName) != 1 || selected.Version != "2" || selected.Sum != fmt.Sprintf("%x", sha256.Sum256(archive)) {
		t.Fatalf("unexpected packages during the bulk: %+v", content.byName)
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
		found := false
		err := filepath.WalkDir(filepath.Join(dir, "cache"), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.Type().IsRegular() || filepath.Ext(path) != ".pkg" {
				return err
			}
			body, err := os.ReadFile(path)
			found = found || bytes.Equal(body, archive)
			return err
		})
		if err != nil || !found {
			t.Fatalf("pkg did not cache the live archive: %v", err)
		}
	})

	// Poudriere's commit replaces .building. Previously advertised paths
	// must still work for clients who refreshed their pkg catalog earlier.
	if err := os.Rename(building, filepath.Join(repoDir, ".real_"+run)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(".real_"+run, "All"), filepath.Join(repoDir, "All")); err != nil {
		t.Fatal(err)
	}
	finalIndex, err := signedPkg(upKey, "packagesite.yaml", liveFixtureCatalogEntry(manifest, localPath, archive))
	if err != nil {
		t.Fatal(err)
	}
	putLiveFixture(t, filepath.Join(repoDir, "packagesite.pkg"), finalIndex)
	if err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cat.PublishLive(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(get(localPath), archive) || !bytes.Equal(get(selected.Path), archive) {
		t.Fatal("a package download changed across the bulk commit")
	}

	later := started.Add(30 * time.Second)
	nextRun := later.Format(buildNameFormat)
	uncommitted := liveFixtureArchive(t, liveFixtureManifest(name, "3"))
	putLiveFixture(t, filepath.Join(building, ".buildname"), []byte(nextRun+"\n"))
	putLiveFixture(t, filepath.Join(building, "All", name+"-3.pkg"), uncommitted)
	putLiveFixture(t, filepath.Join(filepath.Dir(logDir), nextRun, ".poudriere.ports.built"), []byte("devel/"+name+" "+name+"-3 5\n"))
	staging := liveArchiveStore{dir: cat.liveDir, repoDir: repoDir}
	live, err := staging.scan(ctx, cfg, later)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.PublishLive(ctx, live); err != nil {
		t.Fatal(err)
	}
	pkg := cat.current.Load().compiled[name]
	if pkg.Version != "3" {
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
	if cat.current.Load().compiled[name].Version != "2" || !bytes.Equal(get(pkg.Path), uncommitted) {
		t.Fatal("rollback lost the committed package or invalidated an advertised URL")
	}
}
