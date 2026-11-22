package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestWorkerFetchedDependenciesFollowNativeBuild(t *testing.T) {
	cfg := config.DefaultBuilder()
	cfg.BaseFS = t.TempDir()
	repo := cfg.RepositoryDir()
	all := filepath.Join(repo, ".building", "All")
	cache := filepath.Join(repo, ".pkg-cache")
	master := filepath.Join(cfg.BaseFS, "data/logs/bulk", cfg.Jail+"-"+cfg.Ports)
	now := time.Now().UTC()
	run := filepath.Join(master, now.Format(builder.BuildNameFormat))
	prior := filepath.Join(master, now.Add(-time.Hour).Format(builder.BuildNameFormat))
	testutil.WriteFile(t, filepath.Join(repo, ".building", ".buildname"), []byte(now.Format(builder.BuildNameFormat)))
	if err := os.MkdirAll(cache, 0755); err != nil {
		t.Fatal(err)
	}
	add := func(name, version string, deps []string, fromCache bool) string {
		t.Helper()
		wants := make(map[string]runtimeDependency)
		for _, id := range deps {
			n, v := packageinfo.SplitBuildPackage(id)
			wants[n] = runtimeDependency{Origin: "devel/" + n, Version: v}
		}
		data, err := json.Marshal(wants)
		if err != nil {
			t.Fatal(err)
		}
		m := strings.TrimSuffix(testutil.Manifest(name, version), "}") + `,"deps":` + string(data) + "}"
		if name == "wrongarch" {
			m = strings.ReplaceAll(m, "FreeBSD:15:amd64", "FreeBSD:15:aarch64")
		}
		path := filepath.Join(all, name+"-"+version+".pkg")
		testutil.WriteFile(t, path, testutil.Archive(t, m))
		if fromCache {
			if err := os.Link(path, filepath.Join(cache, filepath.Base(path))); err != nil {
				t.Fatal(err)
			}
		}
		return fmt.Sprintf("devel/%s %s-%s\n", name, name, version)
	}
	built := add("firefox", "161.0,2", []string{"dav1d-1.5.5", "intermediate-1", "unproven-1", "loose-1", "wrongarch-1", "wrongorigin-1", "spoofed-1"}, false)
	fetched := add("dav1d", "1.5.5", nil, true) +
		add("intermediate", "1", []string{"leaf-2", "firefox-161.0,2"}, true) +
		add("loose", "1", nil, false) + add("wrongarch", "1", nil, true) +
		add("unused", "1", nil, true) + add("spoofed", "1", nil, false)
	if err := os.Rename(filepath.Join(cache, "dav1d-1.5.5.pkg"), filepath.Join(cache, "dav1d-1.5.5~2$abc3.pkg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dav1d-1.5.5~2$abc3.pkg", filepath.Join(cache, "dav1d-1.5.5.pkg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.building/All/spoofed-1.pkg", filepath.Join(cache, "spoofed-1.pkg")); err != nil {
		t.Fatal(err)
	}
	add("unproven", "1", nil, true)
	add("wrongorigin", "1", nil, true)
	fetched += "other/wrongorigin wrongorigin-1\n"
	retained := add("leaf", "2", nil, true)
	testutil.WriteFile(t, filepath.Join(prior, ".poudriere.ports.fetched"), []byte(retained))
	testutil.WriteFile(t, filepath.Join(run, ".poudriere.ports.built"), []byte(built))
	testutil.WriteFile(t, filepath.Join(run, ".poudriere.ports.fetched"), []byte(fetched))
	baseline := map[string]string{"dav1d": "1.5.4_1", "intermediate": "1"}
	s := LiveArchiveStore{Dir: t.TempDir(), RepoDir: repo, offeredVersion: func(n string) string { return baseline[n] }}
	scan := func() map[string]LivePackage {
		t.Helper()
		p, err := s.Scan(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	got := scan()
	if len(got) != 3 || got["firefox"].File == nil || got["firefox"].Fetched || !got["dav1d"].Fetched || !got["leaf"].Fetched {
		t.Fatalf("expected local Firefox and two proven downloaded dependencies, got %+v", got)
	}
	if entries, err := os.ReadDir(s.Dir); err != nil || len(entries) != 3 {
		t.Fatalf("unexpected archive copies: %v, %v", entries, err)
	}
	if s.retryFetch.IsZero() {
		t.Fatal("did not schedule a retry for the unexpected native cache alias")
	}
	if err := os.Remove(filepath.Join(cache, "spoofed-1.pkg")); err != nil {
		t.Fatal(err)
	}
	scan()
	if !s.retryFetch.IsZero() {
		t.Fatal("retained a retry for an obsolete error")
	}
	// Only the signed baseline changed. This intermediary now also needs to
	// be advertised. The already captured dependency keeps its immutable copy.
	delete(baseline, "intermediate")
	if p := scan(); len(p) != 4 || !p["intermediate"].Fetched || p["dav1d"].File != got["dav1d"].File {
		t.Fatalf("baseline-only change failed to select native dependency: %+v", p)
	}
	if err := os.Remove(filepath.Join(cache, "dav1d-1.5.5.pkg")); err != nil {
		t.Fatal(err)
	}
	if p := scan(); len(p) != 3 || p["dav1d"].File != nil {
		t.Fatalf("dependency remained after native cache link was removed: %+v", p)
	}
}
