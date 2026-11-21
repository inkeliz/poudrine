package poudrine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPathAndWorkerValues(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "settings.json")
	for _, tc := range []struct {
		field  string
		change func(*Configuration)
	}{
		{"service_user", func(c *Configuration) { c.ServiceUser = "-builder" }},
		{"service_user", func(c *Configuration) { c.ServiceUser = "_poudrine_status" }},
		{"build_key", func(c *Configuration) { c.BuildKey = publicStatsFile }},
		{"tls_key", func(c *Configuration) { c.TLSCert = "cert.pem"; c.TLSKey = filepath.Join(c.State, standaloneStatsName) }},
		{"service_user", func(c *Configuration) { c.ServiceUser = "group:member" }},
		{"service_user", func(c *Configuration) { c.ServiceUser = "machine$other" }},
		{"service_user", func(c *Configuration) { c.ServiceUser = strings.Repeat("x", 33) }},
		{"state", func(c *Configuration) { c.State = "/" }},
		{"state", func(c *Configuration) { c.State = "data\tcache" }},
		{"state", func(c *Configuration) { c.State = "settings.json/state" }},
		{"state", func(c *Configuration) { c.State = filepath.Join(c.BaseFS, "jails", "test") }},
		{"state", func(c *Configuration) { c.State = filepath.Join(c.Distfiles, "private") }},
		{"fingerprints", func(c *Configuration) { c.Fingerprints = "   " }},
		{"tls_cert", func(c *Configuration) { c.TLSKey = "key.pem" }},
		{"tls_key", func(c *Configuration) { c.TLSCert = "cert.pem"; c.TLSKey = "keys/" }},
		{"tls_key", func(c *Configuration) { c.TLSCert = "cert.pem"; c.TLSKey = "settings.json.lock" }},
		{"tls_key", func(c *Configuration) { c.State = "data"; c.TLSCert = "cert.pem"; c.TLSKey = "data/repository.key" }},
		{"build_status", func(c *Configuration) { c.BuildStatus = c.PackagesFile }},
		{"build_key", func(c *Configuration) { c.BuildKey = filepath.Join(c.PoudriereConfig, "worker.lock") }},
		{"packages_file", func(c *Configuration) { c.PackagesFile = filepath.Join(c.PoudriereConfig, "poudriere.conf") }},
		{"packages_file", func(c *Configuration) { c.PackagesFile = filepath.Join(c.PoudriereConfig, ".poudrine-pauses") }},
		{"build_key", func(c *Configuration) { c.BuildKey = (BuilderConfig{ConfigDir: c.PoudriereConfig}).makeJobsFile() }},
		{"packages_file", func(c *Configuration) { c.PackagesFile = filepath.Dir(c.BaseFS) }},
		{"packages_file", func(c *Configuration) { c.PackagesFile = c.BaseFS }},
		{"build_status", func(c *Configuration) { c.State = "state"; c.BuildStatus = filepath.Join(root, "state", "build.json") }},
		{"build_status", func(c *Configuration) { c.BuildStatus = configPath }},
		{"build_key", func(c *Configuration) { c.BuildKey = configPath + ".admin.pub" }},
		{"local_repo", func(c *Configuration) { c.LocalRepo = filepath.Join(c.BuildKey, "repo") }},
		{"state", func(c *Configuration) { c.State = strings.Repeat("x", 256) }},
		{"poudriere_config", func(c *Configuration) { c.PoudriereConfig = "worker" }},
		{"jail", func(c *Configuration) { c.Jail = "bad/name" }},
		{"ports", func(c *Configuration) { c.Ports = "bad.tree" }},
		{"release", func(c *Configuration) { c.Release = "0.1-RELEASE" }},
		{"release", func(c *Configuration) { c.Release = strings.Repeat("9", 32) + ".1-RELEASE" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			c := defaultConfiguration()
			tc.change(&c)
			if _, err := c.options(configPath); err == nil || !strings.HasPrefix(err.Error(), tc.field+": ") {
				t.Fatalf("expected error naming %s, got %v", tc.field, err)
			}
		})
	}
}

func TestValidConfigPathLayouts(t *testing.T) {
	for _, name := range []string{"_poudrine", "pkg-builder", "Build.Admin", "svc$"} {
		c := defaultConfiguration()
		c.ServiceUser = name
		c.Release = "15.1-RELEASE-p6"
		c.State = "private state"
		c.LocalRepo = filepath.Join(c.BaseFS, "data", "packages", "stable")
		c.Distfiles = filepath.Join(c.BaseFS, "distfiles")
		c.TLSCert, c.TLSKey = "../certs/site.pem", "../certs/site.pem"
		o, err := c.options("/srv/poudrine/config/poudrine.json")
		if err != nil || o.state != "/srv/poudrine/config/private state" || o.tlsCert != "/srv/poudrine/certs/site.pem" {
			t.Fatalf("valid layout for %s: %v", name, err)
		}
	}
}

func TestHostWorkerStorageAndSymlinks(t *testing.T) {
	// This standard FreeBSD link lets the unprivileged test suite exercise
	// the root-owned path walk without creating or modifying host files.
	const link = "/usr/sbin/sendmail"
	if st, err := os.Lstat(link); err == nil && st.Mode()&os.ModeSymlink != 0 {
		want, err := filepath.EvalSymlinks(link)
		if err != nil {
			t.Fatal(err)
		}
		got, exists, err := inspectRootPath(link, false, true)
		if err != nil || !exists || got != want {
			t.Fatalf("host-managed symlink: %q, %v, %v", got, exists, err)
		}
		if _, err := rootControlled(link, false); err == nil {
			t.Fatal("remote worker edits accepted a symlink")
		}
	}
	o, err := defaultConfiguration().options(defaultConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	o.builder.StatusFile = "/dev/null"
	if err := o.checkWorkerStorage("worker"); err == nil || !strings.HasPrefix(err.Error(), "build_status: ") {
		t.Fatalf("character device accepted for a status file: %v", err)
	}
	if _, _, err := inspectRootPath("/tmp/poudrine-unclaimed/worker", true, true); err == nil {
		t.Fatal("accepted a missing worker directory in a writable parent")
	}
}
