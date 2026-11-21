package poudrine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveCatalogDependencyClosure(t *testing.T) {
	const abi = "freebsd:15:amd64"
	archive := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(archive, []byte("pkg"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	file := &compiledArchive{path: archive, info: info}
	makeManifest := func(name, version string, deps map[string]runtimeDependency) manifest {
		origin := "devel/" + name
		p := CatalogPackage{Name: name, Version: version, Origin: origin, BuildOrigin: origin, ABI: abi, Size: 3, Sum: fmt.Sprintf("%x", sha256.Sum256([]byte("pkg")))}
		raw, err := json.Marshal(map[string]any{"deps": deps})
		if err != nil {
			t.Fatal(err)
		}
		return manifest{pkg: p, raw: raw}
	}
	dep := func(name, version string) map[string]runtimeDependency {
		return map[string]runtimeDependency{name: {Origin: "devel/" + name, Version: version}}
	}
	withLibraries := func(m manifest, provides, requires []string) manifest {
		var runtime packageRuntime
		if err := json.Unmarshal(m.raw, &runtime); err != nil {
			t.Fatal(err)
		}
		runtime.Provides, runtime.Requires = provides, requires
		var err error
		m.raw, err = json.Marshal(runtime)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, test := range []struct {
		name         string
		repo         []manifest
		staged       []manifest
		wantCompiled []string
	}{
		{
			"hold dependent and its parent until a sufficiently new dependency exists",
			[]manifest{makeManifest("leaf", "1", nil)},
			[]manifest{makeManifest("app", "1", dep("lib", "1")), makeManifest("lib", "1", dep("leaf", "2"))}, nil,
		},
		{
			"compatible cycle becomes available together",
			nil, []manifest{makeManifest("app", "1", dep("lib", "2")), makeManifest("lib", "2", dep("app", "1"))},
			[]string{"app", "lib"},
		},
		{
			"missing runtime dependency below an upstream intermediate",
			[]manifest{makeManifest("lib", "2", dep("leaf", "2"))}, []manifest{makeManifest("app", "1", dep("lib", "1"))}, nil,
		},
		{
			"published fallback satisfies parent after staged dependency fails",
			[]manifest{makeManifest("lib", "2", nil)},
			[]manifest{makeManifest("app", "1", dep("lib", "1")), makeManifest("lib", "3", dep("missing", "1"))},
			[]string{"app"},
		},
		{
			"newer FreeBSD dependency remains selected",
			[]manifest{makeManifest("lib", "4", nil)},
			[]manifest{makeManifest("app", "1", dep("lib", "4")), makeManifest("lib", "3", nil)},
			[]string{"app", "lib"},
		},
		{
			"similarly named dependency from wrong origin cannot satisfy the build",
			[]manifest{makeManifest("lib", "2", nil)},
			[]manifest{makeManifest("app", "1", map[string]runtimeDependency{"lib": {Origin: "other/lib", Version: "1"}})}, nil,
		},
		{
			"dependency built for different ABI cannot satisfy the build",
			nil, []manifest{makeManifest("app", "1", dep("other-arch", "1")), makeManifest("other-arch", "1", nil)}, nil,
		},
		{
			"soname bump holds provider and new consumer at the same package version",
			[]manifest{withLibraries(makeManifest("lib", "1", nil), []string{"libfoo.so.1"}, nil)},
			[]manifest{
				withLibraries(makeManifest("lib", "1", nil), []string{"libfoo.so.2"}, nil),
				withLibraries(makeManifest("app", "1", dep("lib", "1")), nil, []string{"libfoo.so.2"}),
				withLibraries(makeManifest("legacy", "1", dep("lib", "1")), nil, []string{"libfoo.so.1"}),
			}, []string{"legacy"},
		},
		{
			"additive provider and client of its new soname become available together",
			[]manifest{withLibraries(makeManifest("lib", "1", nil), []string{"libfoo.so.1"}, nil)},
			[]manifest{
				withLibraries(makeManifest("lib", "2", nil), []string{"libfoo.so.1", "libfoo.so.2"}, nil),
				withLibraries(makeManifest("app", "1", dep("lib", "2")), nil, []string{"libfoo.so.2"}),
			}, []string{"lib", "app"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected := make(map[string]manifest)
			for _, m := range test.repo {
				selected[m.pkg.Name] = m
			}
			live := make(map[string]livePackage)
			for _, m := range test.staged {
				m.pkg.Local = true
				if m.pkg.Name == "other-arch" {
					m.pkg.ABI = "freebsd:15:aarch64"
				}
				live[m.pkg.Name] = livePackage{manifest: m, file: file}
			}
			s := &catalogSnapshot{compiled: map[string]CatalogPackage{}, compiledFiles: map[string]*compiledArchive{}, compiledAt: map[string]time.Time{}, displaced: map[string]CatalogPackage{}}
			mergeLivePackages(live, selected, map[string]bool{abi: true}, s)
			if len(s.compiled) != len(test.wantCompiled) {
				t.Fatalf("compiled = %v, want %v", s.compiled, test.wantCompiled)
			}
			for _, name := range test.wantCompiled {
				if !s.compiled[name].live || s.compiledFiles[name] != file {
					t.Fatalf("missing live compiled package %s", name)
				}
			}
			for _, published := range test.repo {
				if p := selected[published.pkg.Name].pkg; comparePkgVersions(p.Version, published.pkg.Version) < 0 {
					t.Fatalf("downgraded %s from %s to %s", p.Name, published.pkg.Version, p.Version)
				}
			}
		})
	}
}
