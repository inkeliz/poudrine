package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inkeliz/poudrine/internal/packageinfo"
)

func compiledFixture(t *testing.T, p packageinfo.Package, body string) *Snapshot {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compiled.pkg")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return &Snapshot{
		Compiled:      map[string]packageinfo.Package{p.Name: p},
		compiledFiles: map[string]*CompiledArchive{p.Name: {Path: path, Info: stat}},
	}
}
