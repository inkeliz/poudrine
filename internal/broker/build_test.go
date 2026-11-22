package broker

import (
	"slices"
	"testing"

	"github.com/inkeliz/poudrine/internal/builder"
)

func TestRetrySelectsFailedDependenciesAndSkippedPackages(t *testing.T) {
	failed := []builder.BuildIssue{
		{Package: "abseil-1", Origin: "devel/abseil"},
		{Package: "lib-1", Origin: "devel/lib"},
		{Package: "other-1", Origin: "devel/other"},
	}
	skipped := []builder.BuildIssue{
		{Package: "codex-1", Origin: "misc/codex", BlockedBy: "protobuf-2"},
		{Package: "protobuf-2", Origin: "devel/protobuf", BlockedBy: "abseil-1"},
		{Package: "protobuf-2", Origin: "devel/protobuf", BlockedBy: "lib-1"},
	}
	want := []string{"devel/abseil", "devel/lib", "devel/protobuf", "misc/codex"}
	for _, pkg := range []string{"abseil-1", "protobuf-2", "codex-1"} {
		if got := affectedOrigins(failed, skipped, pkg); !slices.Equal(got, want) {
			t.Errorf("retry %s: got %v", pkg, got)
		}
	}
	if got := affectedOrigins(failed, skipped, "unknown-1"); len(got) != 0 {
		t.Errorf("unknown package selected %v", got)
	}
	if got := affectedOrigins(failed, skipped, ""); len(got) != 5 || !slices.Contains(got, "devel/other") {
		t.Errorf("retry entire run: %v", got)
	}
}
