package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildJailRoot(t *testing.T) {
	c := Builder{BaseFS: t.TempDir(), ConfigDir: t.TempDir(), Jail: "latest"}
	if got := c.BuildJailRoot(); got != filepath.Join(c.BaseFS, "jails", c.Jail) {
		t.Fatalf("default jail root = %q", got)
	}
	dir := filepath.Join(c.ConfigDir, "poudriere.d", "jails", c.Jail)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mnt"), []byte("/build/jails/15\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := c.BuildJailRoot(); got != "/build/jails/15" {
		t.Fatalf("custom jail root = %q", got)
	}
}

func TestDefaultCatalogPoll(t *testing.T) {
	o, err := DefaultConfiguration().Options(defaultConfigPath)
	if err != nil || o.Refresh != 5*time.Minute {
		t.Fatalf("default catalog polling interval %s: %v", o.Refresh, err)
	}
}
