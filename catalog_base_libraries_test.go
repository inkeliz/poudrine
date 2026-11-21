package poudrine

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
)

func TestJailBaseLibraries(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"lib", "usr/lib", "usr/lib32", "usr/local/lib", "usr/lib/libdir.so.1"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"lib/libc.so.7", "usr/lib/libz.so.6", "usr/lib32/libc.so.7", "usr/local/lib/libports.so.2", "lib/libfake.a"} {
		if err := os.WriteFile(filepath.Join(root, file), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for alias, target := range map[string]string{
		"usr/lib/libalias.so.1": "../../lib/libc.so.7", "lib/libalias2.so.1": "/usr/lib/libalias.so.1",
		"usr/lib/libbad.so.1": "../local/lib/libports.so.2", "usr/lib32/libwrong.so.1": "../lib/libz.so.6",
		"usr/lib/libdead.so.1": "libnone.so.8", "usr/lib/libcycle.so.1": "libcycle.so.1",
	} {
		if err := os.Symlink(target, filepath.Join(root, alias)); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]bool{"libc.so.7": true, "libalias.so.1": true, "libalias2.so.1": true, "libc.so.7:32": true, "libz.so.6": true}
	if got := jailBaseLibraries(root); !maps.Equal(got, want) {
		t.Fatalf("base libraries: got %v, want %v", got, want)
	}
	if got := jailBaseLibraries(""); len(got) != 0 {
		t.Fatalf("unspecified jail root yielded %v", got)
	}
}

func TestBuildJailRoot(t *testing.T) {
	c := BuilderConfig{BaseFS: t.TempDir(), ConfigDir: t.TempDir(), Jail: "latest"}
	if got := c.buildJailRoot(); got != filepath.Join(c.BaseFS, "jails", c.Jail) {
		t.Fatalf("default jail root = %q", got)
	}
	dir := filepath.Join(c.ConfigDir, "poudriere.d", "jails", c.Jail)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mnt"), []byte("/build/jails/15\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := c.buildJailRoot(); got != "/build/jails/15" {
		t.Fatalf("custom jail root = %q", got)
	}
}
