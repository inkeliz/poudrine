package builder

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestConfiguredOriginsRequiresRegularFile(t *testing.T) {
	if origins := ConfiguredOrigins(t.TempDir()); origins != nil {
		t.Fatalf("directory reported as an inventory: %v", origins)
	}
	dir := t.TempDir()
	list := filepath.Join(dir, "packages.txt")
	if err := os.WriteFile(list, []byte("ftp/curl\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "packages-link")
	if err := os.Symlink(list, link); err != nil {
		t.Fatal(err)
	}
	if origins := ConfiguredOrigins(link); !reflect.DeepEqual(origins, []string{"ftp/curl"}) {
		t.Fatalf("configured symlink: %v", origins)
	}
}

func TestResolvedPortVersions(t *testing.T) {
	cfg := testutil.WorkerConfig(t.TempDir())
	bulk := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports)
	dir := filepath.Join(bulk, "test-build")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("test-build", filepath.Join(bulk, "latest")); err != nil {
		t.Fatal(err)
	}
	const spec = "devel/py-thing@py311"
	if err := os.WriteFile(filepath.Join(dir, ".poudriere.ports.queued"), []byte(spec+" py311-my-thing-2.1_2,1 listed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if p := ResolvedPorts(cfg)["py311-my-thing"]; p != (ResolvedPort{spec, "2.1_2,1"}) {
		t.Fatal(p)
	}
	if err := os.WriteFile(filepath.Join(dir, ".poudriere.all_pkgs%"), []byte("py311-my-thing-2.2 "+spec+" listed\ninvalid\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if p := ResolvedPorts(cfg)["py311-my-thing"]; p != (ResolvedPort{spec, "2.2"}) {
		t.Fatal(p)
	}
}
