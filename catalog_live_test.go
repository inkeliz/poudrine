package poudrine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		var runtime struct {
			packageRuntime
			Provides []string `json:"shlibs_provided"`
		}
		if err := json.Unmarshal(m.raw, &runtime); err != nil {
			t.Fatal(err)
		}
		runtime.Provides, runtime.Requires = provides, requires
		m.provides = provides
		var err error
		m.raw, err = json.Marshal(runtime)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	formula := makeManifest("viewer", "1", nil)
	formula.raw = json.RawMessage(`{"dep_formula":"dav1d >= 1.5.5"}`)
	armLibrary := withLibraries(makeManifest("lib", "1", nil), []string{"libfoo.so.2"}, nil)
	armLibrary.pkg.ABI = "freebsd:15:aarch64"
	for _, test := range []struct {
		name         string
		repo         []manifest
		staged       []manifest
		wantCompiled []string
		holdDetails  map[string]string
	}{
		{
			"transitive legacy dependencies accept an older installed provider",
			[]manifest{makeManifest("leaf", "1", nil)},
			[]manifest{makeManifest("app", "1", dep("lib", "1")), makeManifest("lib", "1", dep("leaf", "2"))},
			[]string{"app", "lib"}, nil,
		},
		{
			"a missing transitive package holds its parent too",
			nil,
			[]manifest{makeManifest("app", "1", dep("lib", "1")), makeManifest("lib", "1", dep("leaf", "2"))}, nil,
			map[string]string{"app": "lib needs leaf", "lib": "needs leaf"},
		},
		{
			"Firefox built with dav1d 1.5.5 accepts the catalog's ABI-compatible 1.5.4_1",
			[]manifest{withLibraries(makeManifest("dav1d", "1.5.4_1", nil), []string{"libdav1d.so.7"}, nil)},
			[]manifest{withLibraries(makeManifest("firefox", "161.0,2", dep("dav1d", "1.5.5")), nil, []string{"libdav1d.so.7"})},
			[]string{"firefox"}, nil,
		},
		{
			"older published provider lacks a required library with no staged alternative",
			[]manifest{withLibraries(makeManifest("lib", "1", nil), []string{"libfoo.so.1"}, nil)},
			[]manifest{withLibraries(makeManifest("app", "1", dep("lib", "2")), nil, []string{"libfoo.so.2"})},
			nil, map[string]string{"app": "needs libfoo.so.2"},
		},
		{
			"FreeBSD base supplies a required library",
			nil, []manifest{withLibraries(makeManifest("app", "1", nil), nil, []string{"libc.so.7"})},
			[]string{"app"}, nil,
		},
		{
			"held provider cannot satisfy an implicit shared library dependency",
			nil, []manifest{
				withLibraries(makeManifest("lib", "2", dep("absent", "1")), []string{"libfoo.so.2"}, nil),
				withLibraries(makeManifest("app", "1", nil), nil, []string{"libfoo.so.2"}),
			}, nil, map[string]string{"lib": "needs absent", "app": "needs libfoo.so.2"},
		},
		{
			"unrelated provider on the wrong ABI cannot satisfy a shared library",
			[]manifest{armLibrary}, []manifest{withLibraries(makeManifest("app", "1", nil), nil, []string{"libfoo.so.2"})},
			nil, map[string]string{"app": "needs libfoo.so.2"},
		},
		{
			"explicit dependency formula waits for native bulk commit",
			[]manifest{makeManifest("dav1d", "1.5.4_1", nil)}, []manifest{formula},
			nil, map[string]string{"viewer": "dependency formula"},
		},
		{
			"compatible cycle becomes available together",
			nil, []manifest{makeManifest("app", "1", dep("lib", "2")), makeManifest("lib", "2", dep("app", "1"))},
			[]string{"app", "lib"}, nil,
		},
		{
			"missing runtime dependency below an upstream intermediate",
			[]manifest{makeManifest("lib", "2", dep("leaf", "2"))}, []manifest{makeManifest("app", "1", dep("lib", "1"))}, nil,
			map[string]string{"app": "lib needs leaf"},
		},
		{
			"published fallback satisfies parent after staged dependency fails",
			[]manifest{makeManifest("lib", "2", nil)},
			[]manifest{makeManifest("app", "1", dep("lib", "1")), makeManifest("lib", "3", dep("missing", "1"))},
			[]string{"app"}, map[string]string{"lib": "needs missing"},
		},
		{
			"newer FreeBSD dependency remains selected",
			[]manifest{makeManifest("lib", "4", nil)},
			[]manifest{makeManifest("app", "1", dep("lib", "4")), makeManifest("lib", "3", nil)},
			[]string{"app", "lib"}, nil,
		},
		{
			"similarly named dependency from wrong origin cannot satisfy the build",
			[]manifest{makeManifest("lib", "2", nil)},
			[]manifest{makeManifest("app", "1", map[string]runtimeDependency{"lib": {Origin: "other/lib", Version: "1"}})}, nil,
			map[string]string{"app": "needs lib from other/lib"},
		},
		{
			"dependency without a recorded origin resolves by its package name",
			[]manifest{makeManifest("lib", "2", nil)},
			[]manifest{makeManifest("app", "1", map[string]runtimeDependency{"lib": {Version: "2"}})}, []string{"app"}, nil,
		},
		{
			"dependency built for different ABI cannot satisfy the build",
			nil, []manifest{makeManifest("app", "1", dep("other-arch", "1")), makeManifest("other-arch", "1", nil)}, nil,
			map[string]string{"app": "needs other-arch", "other-arch": "freebsd:15:aarch64"},
		},
		{
			"soname bump holds provider and new consumer at the same package version",
			[]manifest{withLibraries(makeManifest("lib", "1", nil), []string{"libfoo.so.1"}, nil)},
			[]manifest{
				withLibraries(makeManifest("lib", "1", nil), []string{"libfoo.so.2"}, nil),
				withLibraries(makeManifest("app", "1", dep("lib", "1")), nil, []string{"libfoo.so.2"}),
				withLibraries(makeManifest("legacy", "1", dep("lib", "1")), nil, []string{"libfoo.so.1"}),
			}, []string{"legacy"}, map[string]string{"lib": "removes libfoo.so.1", "app": "needs libfoo.so.2"},
		},
		{
			"additive provider and client of its new soname become available together",
			[]manifest{withLibraries(makeManifest("lib", "1", nil), []string{"libfoo.so.1"}, nil)},
			[]manifest{
				withLibraries(makeManifest("lib", "2", nil), []string{"libfoo.so.1", "libfoo.so.2"}, nil),
				withLibraries(makeManifest("app", "1", dep("lib", "2")), nil, []string{"libfoo.so.2"}),
			}, []string{"lib", "app"}, nil,
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
			mergeLivePackages(live, selected, map[string]bool{abi: true}, map[string]bool{"libc.so.7": true}, s)
			if len(s.liveHolds) != len(test.holdDetails) {
				t.Fatalf("unexpected publication holds: %+v", s.liveHolds)
			}
			for name, detail := range test.holdDetails {
				p := live[name].manifest.pkg
				h := s.liveHolds[p.Name+"-"+p.Version]
				if h.pkg != p || !strings.Contains(h.reason, detail) {
					t.Errorf("hold for %s: %+v, want %q", name, h, detail)
				}
			}
			if len(s.compiled) != len(test.wantCompiled) {
				t.Fatalf("compiled = %v, want %v", s.compiled, test.wantCompiled)
			}
			for _, name := range test.wantCompiled {
				if !s.compiled[name].live || s.compiledFiles[name] != file {
					t.Fatalf("missing live compiled package %s", name)
				}
				served := selected[name].pkg
				built := live[name].manifest.pkg
				if comparePkgVersions(served.Version, built.Version) < 0 || served.Version == built.Version && !served.live {
					t.Errorf("catalog entry for %s = %+v, want live version %s or a newer version", name, served, built.Version)
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
