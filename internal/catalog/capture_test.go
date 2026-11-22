package catalog

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

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestLiveArchiveStorePinsBuildAndPackageIdentity(t *testing.T) {
	cfg := config.DefaultBuilder()
	cfg.BaseFS = t.TempDir()
	started := time.Now().UTC()
	run := started.Format(builder.BuildNameFormat)
	stage := filepath.Join(cfg.RepositoryDir(), ".building")
	logDir := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports, run)
	store := LiveArchiveStore{Dir: t.TempDir(), RepoDir: cfg.RepositoryDir()}

	testutil.WriteFile(t, filepath.Join(stage, ".buildname"), []byte("another-run\n"))
	var built strings.Builder
	add := func(id, origin, manifest string) []byte {
		t.Helper()
		archive := testutil.Archive(t, manifest)
		testutil.WriteFile(t, filepath.Join(stage, "All", id+".pkg"), archive)
		fmt.Fprintf(&built, "%s %s 5\n", origin, id)
		return archive
	}
	extra := `,"annotations":{"flavor":"py312","subpackage":"docs"},"future_field":[1,2,3]}`
	good := add("wheel-1", "devel/wheel~docs", strings.TrimSuffix(testutil.Manifest("wheel", "1"), "}")+extra)
	add("mismatch-1", "devel/mismatch", testutil.Manifest("mismatch", "2"))
	add("wrongflavor-1", "devel/wrongflavor@py313~docs", strings.TrimSuffix(testutil.Manifest("wrongflavor", "1"), "}")+extra)
	add("cached-1", "devel/cached", testutil.Manifest("cached", "1"))
	if err := os.MkdirAll(filepath.Join(store.RepoDir, ".pkg-cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(stage, "All", "cached-1.pkg"), filepath.Join(store.RepoDir, ".pkg-cache", "cached-1.pkg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("wheel-1.pkg", filepath.Join(stage, "All", "link-1.pkg")); err != nil {
		t.Fatal(err)
	}
	built.WriteString("devel/link link-1 5\n")
	testutil.WriteFile(t, filepath.Join(logDir, ".poudriere.ports.built"), []byte(built.String()))
	if pubs, err := store.Scan(t.Context(), cfg); err == nil || len(pubs) != 0 {
		t.Fatalf("staging belongs to a different run: %v, %v", pubs, err)
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected capture: %v, %v", entries, err)
	}

	testutil.WriteFile(t, filepath.Join(stage, ".buildname"), []byte(run+"\n"))
	scan := func(store *LiveArchiveStore) LivePackage {
		t.Helper()
		pubs, err := store.Scan(t.Context(), cfg)
		if err != nil || len(pubs) != 1 || pubs["wheel"].File == nil {
			t.Fatalf("expected only the matching success: %v, %v", pubs, err)
		}
		return pubs["wheel"]
	}
	pub := scan(&store)
	sum := fmt.Sprintf("%x", sha256.Sum256(good))
	p := pub.Manifest.Pkg
	if p.BuildOrigin != "devel/wheel@py312~docs" || p.Path != "All/poudrine/"+sum+".pkg" || p.Sum != sum || p.Size != int64(len(good)) {
		t.Fatalf("incorrect package manifest: %+v", p)
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(pub.Manifest.Raw, &entry); err != nil || string(entry["future_field"]) != "[1,2,3]" || !bytes.Equal(entry["repopath"], entry["path"]) {
		t.Fatalf("lost package metadata: %s, %v", pub.Manifest.Raw, err)
	}
	entries, err = os.ReadDir(store.Dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != sum+".pkg" {
		t.Fatalf("unexpected staged files: %v, %v", entries, err)
	}

	restart := LiveArchiveStore{Dir: store.Dir, RepoDir: store.RepoDir}
	again := scan(&restart)
	if !os.SameFile(pub.File.Info, again.File.Info) {
		t.Fatal("restart replaced verified package file")
	}
	if err := os.Rename(stage, filepath.Join(store.RepoDir, ".real_1")); err != nil {
		t.Fatal(err)
	}
	if pubs, err := restart.Scan(t.Context(), cfg); err != nil || len(pubs) != 0 {
		t.Fatalf("withdrew native stage but retained catalog entries: %v %v", pubs, err)
	}
	if err := again.File.verify(p); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverRetainedFirefoxAfterWorkerRestart(t *testing.T) {
	cfg := config.DefaultBuilder()
	cfg.BaseFS = t.TempDir()
	stage := filepath.Join(cfg.RepositoryDir(), ".building")
	all := filepath.Join(stage, "All")
	master := filepath.Join(cfg.BaseFS, "data/logs/bulk", cfg.Jail+"-"+cfg.Ports)
	old := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	next := old.Add(6 * time.Hour)
	stamp := old.Add(time.Hour)
	id := "firefox-161.0,2"
	m := strings.Replace(testutil.Manifest("firefox", "161.0,2"), "devel/firefox", "www/firefox", 1)
	m = strings.TrimSuffix(m, "}") + fmt.Sprintf(`,"annotations":{"build_timestamp":%q}}`, stamp.Format(poudriereTimestamp))
	archive := testutil.Archive(t, m)
	hashed := "Hashed/" + id + "~2$c8xkqcud.pkg"
	testutil.WriteFile(t, filepath.Join(all, hashed), archive)
	if err := os.Symlink(hashed, filepath.Join(all, id+".pkg")); err != nil {
		t.Fatal(err)
	}
	oldRun := filepath.Join(master, old.Format(builder.BuildNameFormat))
	testutil.WriteFile(t, filepath.Join(oldRun, ".poudriere.ports.built"), []byte("www/firefox "+id+" 179\n"))
	jobLog := filepath.Join(oldRun, "logs", id+".log")
	writeStart := func(st time.Time) {
		testutil.WriteFile(t, jobLog, []byte("build started at "+st.Format(time.UnixDate)+"\n"))
	}
	testutil.WriteFile(t, filepath.Join(stage, ".buildname"), []byte(next.Format(builder.BuildNameFormat)+"\n"))
	testutil.WriteFile(t, filepath.Join(master, next.Format(builder.BuildNameFormat), ".poudriere.ports.built"), nil)

	restart := func() *LiveArchiveStore { return &LiveArchiveStore{Dir: t.TempDir(), RepoDir: cfg.RepositoryDir()} }
	scan := func(s *LiveArchiveStore, want int) {
		t.Helper()
		got, err := s.Scan(t.Context(), cfg)
		if err != nil || len(got) != want {
			t.Fatalf("recovered %v, expected %d packages: %v", got, want, err)
		}
		if want != 0 && got["firefox"].Manifest.Pkg.Version != "161.0,2" {
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
	testutil.WriteFile(t, replacement, testutil.Archive(t, modified))
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
	cfg := config.DefaultBuilder()
	cfg.BaseFS = t.TempDir()
	repo := cfg.RepositoryDir()
	stage := filepath.Join(repo, ".building")
	master := filepath.Join(cfg.BaseFS, "data/logs/bulk", cfg.Jail+"-"+cfg.Ports)
	old := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	stamp := old.Add(time.Minute)
	now := old.Add(time.Hour)
	const id = "dav1d-1.5.5"
	const origin = "multimedia/dav1d"
	prev := filepath.Join(master, old.Format(builder.BuildNameFormat))
	testutil.WriteFile(t, filepath.Join(prev, ".poudriere.ports.built"), []byte(origin+" "+id+" 15\n"))
	testutil.WriteFile(t, filepath.Join(prev, "logs", id+".log"), []byte("build started at "+stamp.Format(time.UnixDate)+"\n"))
	testutil.WriteFile(t, filepath.Join(stage, ".buildname"), []byte(now.Format(builder.BuildNameFormat)+"\n"))
	testutil.WriteFile(t, filepath.Join(master, now.Format(builder.BuildNameFormat), ".poudriere.ports.built"), nil)
	all := filepath.Join(stage, "All")
	if err := os.MkdirAll(all, 0700); err != nil {
		t.Fatal(err)
	}
	baseline := "1.5.4_1"
	store := LiveArchiveStore{Dir: t.TempDir(), RepoDir: repo, offeredVersion: func(string) string { return baseline }}
	scan := func(want bool) {
		t.Helper()
		live, err := store.Scan(t.Context(), cfg)
		p, ok := live["dav1d"]
		if err != nil || ok != want || ok && (p.File == nil || p.Manifest.Pkg.Version != "1.5.5") {
			t.Fatalf("want retained dav1d %t, got %+v: %v", want, p, err)
		}
	}
	scan(false)
	// An older run proves the build. Seeing the finished file for the first
	// time on a later poll must work even when the native index is behind.
	m := strings.TrimSuffix(testutil.OriginManifest("dav1d", origin, "1.5.5"), "}")
	m += fmt.Sprintf(`,"annotations":{"build_timestamp":%q}}`, stamp.Format(poudriereTimestamp))
	committed := filepath.Join(repo, "All", id+".pkg")
	testutil.WriteFile(t, committed, testutil.Archive(t, m))
	if err := os.Link(committed, filepath.Join(all, id+".pkg")); err != nil {
		t.Fatal(err)
	}
	scan(true)
	// A committed catalog at the correct version needs no retained copy.
	// If that catalog falls back while the stage is unchanged, re-check it.
	baseline = "1.5.5"
	store = LiveArchiveStore{Dir: t.TempDir(), RepoDir: repo, offeredVersion: func(string) string { return baseline }}
	scan(false)
	baseline = "1.5.4_1"
	scan(true)
}
