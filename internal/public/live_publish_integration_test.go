package public

import (
	"testing"
)

func TestLivePackageInstallBeforeBulkCommit(t *testing.T) {
	for _, source := range []string{"upstream", "built", "fetched"} {
		t.Run(source, func(t *testing.T) { testLivePackageInstall(t, source) })
	}
}
