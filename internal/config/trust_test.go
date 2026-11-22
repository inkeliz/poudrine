package config

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestRootConfigTrust(t *testing.T) {
	if err := CheckRootConfigPath("/etc/passwd"); err != nil {
		t.Fatal(err)
	}
	if err := CheckRootConfigPath("/tmp/poudrine-unowned-config.json"); err == nil {
		t.Fatal("trusted missing file below world writable directory")
	}
}

func TestWorkerPathsFromWebSettings(t *testing.T) {
	old := DefaultConfiguration()
	path := "/usr/local/etc/poudrine.json"
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := CheckBrokerSettings(cancelled, path, old, old); !errors.Is(err, context.Canceled) {
		t.Fatalf("preflight with cancelled supervisor: %v", err)
	}
	for _, dest := range []string{"/mnt/poudrine-test/worker", "/media/storage/poudrine"} {
		c := old
		c.BaseFS = dest
		if err := CheckBrokerSettings(t.Context(), path, old, c); err != nil {
			t.Fatalf("%s: %v", dest, err)
		}
	}
	for _, change := range []func(*Configuration){
		func(c *Configuration) { c.BuildStatus = "/mnt/poudrine-test/hook.sh" },
		func(c *Configuration) { c.PackagesFile = "/etc/passwd" },
		func(c *Configuration) { c.State = "/etc/rc.d/new" },
		func(c *Configuration) { c.BuildStatus = filepath.Join(c.BaseFS, "jails", "status.json") },
		func(c *Configuration) { c.ServiceUser = "root" },
	} {
		c := old
		change(&c)
		if err := CheckBrokerSettings(t.Context(), path, old, c); err == nil {
			t.Fatalf("accepted unsafe settings %+v", c)
		}
	}
}
