package builder

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestOverlayWorkerBuildAndRemoval(t *testing.T) {
	root := t.TempDir()
	cfg := testutil.WorkerConfig(root)
	cfg.PackageOverlays = config.PackageOverlays{"databases/mydumper": {DefaultVersions: map[string]string{"mysql": "12.3m"}}}
	b, err := NewBuilder(cfg, func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.configure(); err != nil {
		t.Fatal(err)
	}
	wantOrigin := "databases/mydumper@all\n"
	if roots, err := b.syncOrigins(nil); err != nil || roots != wantOrigin {
		t.Fatalf("effective roots %q: %v", roots, err)
	}
	if roots, err := readPackagesFile(cfg.PackagesFile); err != nil || roots != "" {
		t.Fatalf("overlay persisted in administrator's package list %q: %v", roots, err)
	}
	if roots := ConfiguredOrigins(cfg.BuildListFile()); !slices.Equal(roots, []string{"databases/mydumper@all"}) {
		t.Fatalf("Poudriere package list: %q", roots)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	calls := filepath.Join(root, "calls")
	t.Setenv("BUILD_TEST_CALLS", calls)
	script := "#!/bin/sh\necho \"$*\" >> \"$BUILD_TEST_CALLS\"\n"
	if err := os.WriteFile(filepath.Join(root, "poudriere"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := b.bulk(t.Context(), now, cfg.BuildListFile(), true); err != nil {
		t.Fatal(err)
	}
	// Model the registration made by poudriere ports -c -m null.
	registry := filepath.Join(cfg.ConfigDir, "poudriere.d", "ports", config.OverlayTreeName)
	if err := os.MkdirAll(registry, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, "mnt"), []byte(cfg.OverlayDir()+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := b.bulk(t.Context(), now.Add(time.Second), cfg.BuildListFile(), false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], "ports -c -p "+config.OverlayTreeName+" -m null -M "+cfg.OverlayDir()) ||
		!strings.Contains(lines[1], "-O "+config.OverlayTreeName+" -b latest -f "+cfg.BuildListFile()) ||
		!strings.Contains(lines[2], "-O "+config.OverlayTreeName+" -f "+cfg.BuildListFile()) {
		t.Fatalf("initial and retry bulk invocations: %s", raw)
	}
	unsafe := cfg
	unsafe.SigningKey = filepath.Join(cfg.OverlayDir(), "signing.key")
	if err := unsafe.Validate(); err == nil {
		t.Fatal("accepted signing key inside the tree mounted in build jails")
	}
	if err := os.WriteFile(filepath.Join(cfg.OverlayDir(), "unrelated"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.registerOverlay(t.Context()); err == nil || !strings.Contains(err.Error(), "unexpected entry") {
		t.Fatalf("worker would expose an unrelated file in the jail: %v", err)
	}
	b.Cfg.PackageOverlays = nil
	if err := b.configure(); err != nil {
		t.Fatal(err)
	}
	if roots, err := b.syncOrigins(nil); err != nil || roots != "" || b.Cfg.BuildListFile() != cfg.PackagesFile {
		t.Fatalf("removed overlay still a build root %q: %v", roots, err)
	}
	if _, err := os.Stat(cfg.MakeJobsFile()); !os.IsNotExist(err) {
		t.Fatalf("removed overlay still configured: %v", err)
	}
}

// With a local ports checkout this also exercises real dependency resolution
// and the values Poudriere and pkg read when matching and packaging archives.
func TestOverlayAgainstFreeBSDPorts(t *testing.T) {
	ports := os.Getenv("POUDRINE_TEST_PORTSDIR")
	if ports == "" {
		t.Skip("requires POUDRINE_TEST_PORTSDIR containing databases/mydumper")
	}
	cfg := testutil.WorkerConfig(t.TempDir())
	o := config.PackageOverlay{DefaultVersions: map[string]string{"mysql": "12.3m"}}
	cfg.PackageOverlays = config.PackageOverlays{"databases/mydumper": o}
	b := &Builder{Cfg: cfg}
	if err := b.configure(); err != nil {
		t.Fatal(err)
	}
	var portVariables []string
	makeQuery := func(origin string, vars ...string) ([]byte, error) {
		args := []string{"-C", filepath.Join(ports, origin), "__MAKE_CONF=" + cfg.MakeJobsFile(), "PORTSDIR=" + ports, "OVERLAYS=" + cfg.OverlayDir()}
		args = append(args, portVariables...)
		for _, v := range vars {
			args = append(args, "-V", v)
		}
		return exec.Command("/usr/bin/make", args...).CombinedOutput()
	}
	query := func(origin string, vars ...string) []string {
		t.Helper()
		out, err := makeQuery(origin, vars...)
		if err != nil {
			t.Fatalf("make %s: %v: %s", origin, err, out)
		}
		return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	}
	read := query("databases/mydumper", "MYSQL_DEFAULT", "LIB_DEPENDS", "SELECTED_OPTIONS", "COMPLETE_OPTIONS_LIST", "PORT_OPTIONS", "PKG_NOTES", "PKG_NOTE_"+config.OverlayAnnotation)
	marker := "POUDRINE_OVERLAY_" + strings.ToUpper(o.Digest())
	if len(read) != 7 || read[0] != "12.3m" || !strings.Contains(read[1], "databases/mariadb123-client") ||
		!slices.Contains(strings.Fields(read[2]), marker) || !slices.Contains(strings.Fields(read[3]), marker) ||
		!slices.Contains(strings.Fields(read[4]), marker) || !slices.Contains(strings.Fields(read[5]), config.OverlayAnnotation) || read[6] != o.Digest() {
		t.Fatalf("Ports did not receive the overlay: %q", read)
	}
	b.Cfg.PackageOverlays = config.PackageOverlays{"misc/other": o}
	if err := b.configure(); err != nil {
		t.Fatal(err)
	}
	other := query("databases/mydumper", "MYSQL_DEFAULT", "SELECTED_OPTIONS", "PKG_NOTES")
	if len(other) != 3 || other[0] == "12.3m" || strings.Contains(other[1], marker) || strings.Contains(other[2], config.OverlayAnnotation) {
		t.Fatalf("overlay leaked into a different port: %q", other)
	}
	b.Cfg.PackageOverlays = config.PackageOverlays{"databases/mydumper": {OptionsSet: []string{"MISSPELLED"}}}
	if err := b.configure(); err != nil {
		t.Fatal(err)
	}
	out, err := makeQuery("databases/mydumper", "PKG_NOTES")
	if err == nil || !strings.Contains(string(out), "databases/mydumper: unknown option MISSPELLED") {
		t.Fatalf("ineffective overlay option accepted: %v, %s", err, out)
	}
	// Supply a small option set to the actual framework, including one
	// maintainer default and one option implied by the enabled option.
	portVariables = []string{"OPTIONS_DEFINE=FIRST SECOND THIRD", "OPTIONS_DEFAULT=THIRD", "FIRST_IMPLIES=SECOND"}
	b.Cfg.PackageOverlays = config.PackageOverlays{"databases/mydumper": {OptionsSet: []string{"FIRST"}, OptionsUnset: []string{"THIRD"}}}
	if err := b.configure(); err != nil {
		t.Fatal(err)
	}
	onOff := query("databases/mydumper", "SELECTED_OPTIONS", "DESELECTED_OPTIONS")
	if len(onOff) != 2 || !slices.Contains(strings.Fields(onOff[0]), "FIRST") || !slices.Contains(strings.Fields(onOff[1]), "THIRD") {
		t.Fatalf("explicit enable/disable failed: %q", onOff)
	}
	b.Cfg.PackageOverlays = config.PackageOverlays{"databases/mydumper": {OptionsSet: []string{"FIRST"}, OptionsUnset: []string{"SECOND"}}}
	if err := b.configure(); err != nil {
		t.Fatal(err)
	}
	out, err = makeQuery("databases/mydumper", "PKG_NOTES")
	if err == nil || !strings.Contains(string(out), "databases/mydumper: could not disable SECOND") {
		t.Fatalf("implied option silently overrode the requested settings: %v, %s", err, out)
	}
}
