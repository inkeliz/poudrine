package packageinfo

import (
	"strings"
	"testing"
)

func TestSplitInventoryBuildPackage(t *testing.T) {
	for _, tc := range []struct{ id, name, version string }{
		{"firefox-161.0,2", "firefox", "161.0,2"},
		{"py311-more-itertools-10.8.0_2,1", "py311-more-itertools", "10.8.0_2,1"},
		{"firefox", "", ""}, {"-1", "", ""}, {"firefox-", "", ""},
		{"www/firefox-161.0,2", "", ""}, {strings.Repeat("x", 255) + "-1", "", ""},
	} {
		name, version := SplitBuildPackage(tc.id)
		if name != tc.name || version != tc.version {
			t.Errorf("%q split as %q, %q", tc.id, name, version)
		}
	}
}
