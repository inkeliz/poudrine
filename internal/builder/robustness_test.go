package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestScanBuildResultsLongLines(t *testing.T) {
	dir := t.TempDir()
	long := "devel/long long-1 build " + strings.Repeat("reason", 2000) + "\n"
	oversized := "devel/huge huge-1 build " + strings.Repeat("x", 1<<20) + "\n"
	testutil.WriteFile(t, filepath.Join(dir, ".poudriere.ports.failed"), []byte(long+oversized+"devel/next next-1 build failed\n"+"devel/partial partial-1"))
	run, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	var packages []string
	err = ScanBuildResults(t.Context(), run, "failed", func(_ string, fields []string) bool { packages = append(packages, fields[1]); return true })
	if err != nil || strings.Join(packages, ",") != "long-1,next-1" {
		t.Fatalf("long line stopped scan or partial record exposed: %v, %v", packages, err)
	}
}
