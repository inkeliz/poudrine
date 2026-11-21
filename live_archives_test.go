package poudrine

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveArchiveStorePinsBuildAndPackageIdentity(t *testing.T) {
	cfg := DefaultBuilderConfig()
	cfg.BaseFS = t.TempDir()
	started := time.Now().UTC()
	run := started.Format(buildNameFormat)
	stage := filepath.Join(cfg.RepositoryDir(), ".building")
	logDir := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports, run)
	store := liveArchiveStore{dir: t.TempDir(), repoDir: cfg.RepositoryDir()}

	putLiveFixture(t, filepath.Join(stage, ".buildname"), []byte("another-run\n"))
	var built strings.Builder
	add := func(id, origin, manifest string) []byte {
		t.Helper()
		archive := liveFixtureArchive(t, manifest)
		putLiveFixture(t, filepath.Join(stage, "All", id+".pkg"), archive)
		fmt.Fprintf(&built, "%s %s 5\n", origin, id)
		return archive
	}
	extra := `,"annotations":{"flavor":"py312","subpackage":"docs"},"future_field":[1,2,3]}`
	good := add("wheel-1", "devel/wheel~docs", strings.TrimSuffix(liveFixtureManifest("wheel", "1"), "}")+extra)
	add("mismatch-1", "devel/mismatch", liveFixtureManifest("mismatch", "2"))
	add("wrongflavor-1", "devel/wrongflavor@py313~docs", strings.TrimSuffix(liveFixtureManifest("wrongflavor", "1"), "}")+extra)
	add("cached-1", "devel/cached", liveFixtureManifest("cached", "1"))
	if err := os.MkdirAll(filepath.Join(store.repoDir, ".pkg-cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(stage, "All", "cached-1.pkg"), filepath.Join(store.repoDir, ".pkg-cache", "cached-1.pkg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("wheel-1.pkg", filepath.Join(stage, "All", "link-1.pkg")); err != nil {
		t.Fatal(err)
	}
	built.WriteString("devel/link link-1 5\n")
	putLiveFixture(t, filepath.Join(logDir, ".poudriere.ports.built"), []byte(built.String()))
	if pubs, err := store.scan(t.Context(), cfg, started); err != nil || len(pubs) != 0 {
		t.Fatalf("staging belongs to a different run: %v, %v", pubs, err)
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected capture: %v, %v", entries, err)
	}

	putLiveFixture(t, filepath.Join(stage, ".buildname"), []byte(run+"\n"))
	scan := func(store *liveArchiveStore) livePackage {
		t.Helper()
		pubs, err := store.scan(t.Context(), cfg, started)
		if err != nil || len(pubs) != 1 || pubs["wheel"].file == nil {
			t.Fatalf("expected only the matching success: %v, %v", pubs, err)
		}
		return pubs["wheel"]
	}
	pub := scan(&store)
	sum := fmt.Sprintf("%x", sha256.Sum256(good))
	p := pub.manifest.pkg
	if p.BuildOrigin != "devel/wheel@py312~docs" || p.Path != "All/poudrine/"+sum+".pkg" || p.Sum != sum || p.Size != int64(len(good)) {
		t.Fatalf("incorrect package manifest: %+v", p)
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(pub.manifest.raw, &entry); err != nil || string(entry["future_field"]) != "[1,2,3]" || !bytes.Equal(entry["repopath"], entry["path"]) {
		t.Fatalf("lost package metadata: %s, %v", pub.manifest.raw, err)
	}
	entries, err = os.ReadDir(store.dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != sum+".pkg" {
		t.Fatalf("unexpected staged files: %v, %v", entries, err)
	}

	restart := liveArchiveStore{dir: store.dir, repoDir: store.repoDir}
	again := scan(&restart)
	if !os.SameFile(pub.file.info, again.file.info) {
		t.Fatal("restart replaced verified package file")
	}
	if err := os.Rename(stage, filepath.Join(store.repoDir, ".real_1")); err != nil {
		t.Fatal(err)
	}
	again = scan(&restart)
	if err := again.file.verify(p); err != nil {
		t.Fatal(err)
	}
	if pubs, err := restart.scan(t.Context(), cfg, started.Add(time.Minute)); err != nil || len(pubs) != 0 {
		t.Fatalf("a new run inherited the old capture: %v, %v", pubs, err)
	}
}
