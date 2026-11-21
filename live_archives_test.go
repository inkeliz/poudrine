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
	if pubs, err := store.scan(t.Context(), cfg); err == nil || len(pubs) != 0 {
		t.Fatalf("staging belongs to a different run: %v, %v", pubs, err)
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected capture: %v, %v", entries, err)
	}

	putLiveFixture(t, filepath.Join(stage, ".buildname"), []byte(run+"\n"))
	scan := func(store *liveArchiveStore) livePackage {
		t.Helper()
		pubs, err := store.scan(t.Context(), cfg)
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
	if pubs, err := restart.scan(t.Context(), cfg); err != nil || len(pubs) != 0 {
		t.Fatalf("withdrew native stage but retained catalog entries: %v %v", pubs, err)
	}
	if err := again.file.verify(p); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverRetainedFirefoxAfterWorkerRestart(t *testing.T) {
	cfg := DefaultBuilderConfig()
	cfg.BaseFS = t.TempDir()
	stage := filepath.Join(cfg.RepositoryDir(), ".building")
	all := filepath.Join(stage, "All")
	master := filepath.Join(cfg.BaseFS, "data/logs/bulk", cfg.Jail+"-"+cfg.Ports)
	old := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	next := old.Add(6 * time.Hour)
	stamp := old.Add(time.Hour)
	id := "firefox-161.0,2"
	m := strings.Replace(liveFixtureManifest("firefox", "161.0,2"), "devel/firefox", "www/firefox", 1)
	m = strings.TrimSuffix(m, "}") + fmt.Sprintf(`,"annotations":{"build_timestamp":%q}}`, stamp.Format(poudriereTimestamp))
	archive := liveFixtureArchive(t, m)
	hashed := "Hashed/" + id + "~2$c8xkqcud.pkg"
	putLiveFixture(t, filepath.Join(all, hashed), archive)
	if err := os.Symlink(hashed, filepath.Join(all, id+".pkg")); err != nil {
		t.Fatal(err)
	}
	oldRun := filepath.Join(master, old.Format(buildNameFormat))
	putLiveFixture(t, filepath.Join(oldRun, ".poudriere.ports.built"), []byte("www/firefox "+id+" 179\n"))
	jobLog := filepath.Join(oldRun, "logs", id+".log")
	writeStart := func(st time.Time) {
		putLiveFixture(t, jobLog, []byte("build started at "+st.Format(time.UnixDate)+"\n"))
	}
	putLiveFixture(t, filepath.Join(stage, ".buildname"), []byte(next.Format(buildNameFormat)+"\n"))
	putLiveFixture(t, filepath.Join(master, next.Format(buildNameFormat), ".poudriere.ports.built"), nil)

	restart := func() *liveArchiveStore { return &liveArchiveStore{dir: t.TempDir(), repoDir: cfg.RepositoryDir()} }
	scan := func(s *liveArchiveStore, want int) {
		t.Helper()
		got, err := s.scan(t.Context(), cfg)
		if err != nil || len(got) != want {
			t.Fatalf("recovered %v, expected %d packages: %v", got, want, err)
		}
		if want != 0 && got["firefox"].manifest.pkg.Version != "161.0,2" {
			t.Fatalf("unexpected version: %v", got)
		}
	}
	writeStart(stamp.Add(-time.Hour))
	scan(restart(), 0) // A success for another build of the same version.
	writeStart(stamp.Add(-time.Second))
	s := restart()
	scan(s, 1)
	scan(s, 1) // Unchanged paused stage.
	replacement := filepath.Join(all, "Hashed", "replacement")
	modified := strings.Replace(m, stamp.Format(poudriereTimestamp), old.Format(poudriereTimestamp), 1)
	putLiveFixture(t, replacement, liveFixtureArchive(t, modified))
	if err := os.Rename(replacement, filepath.Join(all, hashed)); err != nil {
		t.Fatal(err)
	}
	scan(s, 0) // The alias is unchanged but its target was replaced.
	if err := os.Remove(filepath.Join(all, id+".pkg")); err != nil {
		t.Fatal(err)
	}
	scan(s, 0) // The old log cannot revive an archive removed from the stage.
	if err := os.Symlink("../elsewhere/"+filepath.Base(hashed), filepath.Join(all, id+".pkg")); err != nil {
		t.Fatal(err)
	}
	scan(restart(), 0)
}

func TestRecoverRetainedDav1dWhenArchiveAppears(t *testing.T) {
	cfg := DefaultBuilderConfig()
	cfg.BaseFS = t.TempDir()
	repo := cfg.RepositoryDir()
	stage := filepath.Join(repo, ".building")
	master := filepath.Join(cfg.BaseFS, "data/logs/bulk", cfg.Jail+"-"+cfg.Ports)
	old := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	stamp := old.Add(time.Minute)
	now := old.Add(time.Hour)
	const id = "dav1d-1.5.5"
	const origin = "multimedia/dav1d"
	prev := filepath.Join(master, old.Format(buildNameFormat))
	putLiveFixture(t, filepath.Join(prev, ".poudriere.ports.built"), []byte(origin+" "+id+" 15\n"))
	putLiveFixture(t, filepath.Join(prev, "logs", id+".log"), []byte("build started at "+stamp.Format(time.UnixDate)+"\n"))
	putLiveFixture(t, filepath.Join(stage, ".buildname"), []byte(now.Format(buildNameFormat)+"\n"))
	putLiveFixture(t, filepath.Join(master, now.Format(buildNameFormat), ".poudriere.ports.built"), nil)
	all := filepath.Join(stage, "All")
	if err := os.MkdirAll(all, 0700); err != nil {
		t.Fatal(err)
	}
	baseline := "1.5.4_1"
	store := liveArchiveStore{dir: t.TempDir(), repoDir: repo, offeredVersion: func(string) string { return baseline }}
	scan := func(want bool) {
		t.Helper()
		live, err := store.scan(t.Context(), cfg)
		p, ok := live["dav1d"]
		if err != nil || ok != want || ok && (p.file == nil || p.manifest.pkg.Version != "1.5.5") {
			t.Fatalf("want retained dav1d %t, got %+v: %v", want, p, err)
		}
	}
	scan(false)
	// An older run proves the build. Seeing the finished file for the first
	// time on a later poll must work even when the native index is behind.
	m := strings.TrimSuffix(liveFixtureOriginManifest("dav1d", origin, "1.5.5"), "}")
	m += fmt.Sprintf(`,"annotations":{"build_timestamp":%q}}`, stamp.Format(poudriereTimestamp))
	committed := filepath.Join(repo, "All", id+".pkg")
	putLiveFixture(t, committed, liveFixtureArchive(t, m))
	if err := os.Link(committed, filepath.Join(all, id+".pkg")); err != nil {
		t.Fatal(err)
	}
	scan(true)
	// A committed catalog at the correct version needs no retained copy.
	// If that catalog falls back while the stage is unchanged, re-check it.
	baseline = "1.5.5"
	store = liveArchiveStore{dir: t.TempDir(), repoDir: repo, offeredVersion: func(string) string { return baseline }}
	scan(false)
	baseline = "1.5.4_1"
	scan(true)
}
