package config

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestPackageOverlayConfiguration(t *testing.T) {
	cfg, err := DecodeConfiguration([]byte(`{"package_overlays":{
		"databases/mydumper":{"default_versions":{"mysql":"10.11m"}},
		"devel/test":{"options_set":["NLS","7ZIP"],"options_unset":["DOCS"]},
		"www/plain":{}
	}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	o, err := cfg.Options("")
	if err != nil {
		t.Fatal(err)
	}
	if opts := o.Builder.PackageOverlays; len(opts) != 3 || opts["databases/mydumper"].DefaultVersions["mysql"] != "10.11m" ||
		!reflect.DeepEqual(opts["devel/test"].OptionsUnset, []string{"DOCS"}) {
		t.Fatalf("lost worker overlays: %#v", opts)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := DecodeConfiguration(encoded, "")
	if err != nil || !reflect.DeepEqual(snapshot, cfg) {
		t.Fatalf("configuration snapshot lost settings: %#v, %v", snapshot, err)
	}
	for _, overlay := range []string{
		`[]`, `null`, `{"devel/test":null}`, `{"devel/test":[]}`,
		`{"devel/test":{},"devel/test":{}}`, `{"../test":{}}`, `{"devel/test@a":{}}`, `{"devel/test~b":{}}`,
		`{"devel/test":{"other":{}}}`, `{"devel/test":{"OPTIONS_SET":[]}}`, `{"devel/test":{"options_set":[],"options_set":[]}}`,
		`{"devel/test":{"options_set":null}}`, `{"devel/test":{"options_set":[null]}}`, `{"devel/test":{"options_set":["DOCS","DOCS"]}}`,
		`{"devel/test":{"options_set":["DOCS"],"options_unset":["DOCS"]}}`, `{"devel/test":{"options_set":["DOCS\nBAD"]}}`,
		`{"devel/test":{"default_versions":null}}`, `{"devel/test":{"default_versions":{"mysql":null}}}`,
		`{"devel/test":{"default_versions":{"mysql":"10.11m","mysql":"11.4m"}}}`,
		`{"devel/test":{"default_versions":{"mysql":"10.11m","MySQL":"11.4m"}}}`,
		`{"devel/test":{"default_versions":{"mysql":"10.11m extra"}}}`, `{"devel/test":{"default_versions":{"mysql":"../foo"}}}`,
		`{"devel/test":{"default_versions":{"my/sl":"10"}}}`, `{"devel/test":{"default_versions":{"mysql":42}}}`,
	} {
		_, err := DecodeConfiguration([]byte(`{"package_overlays":`+overlay+"}"), "")
		if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "package_overlays:") {
			t.Errorf("invalid overlay %s returned %v", overlay, err)
		}
	}
}

func TestOverlayDigest(t *testing.T) {
	a := PackageOverlay{OptionsSet: []string{"NLS", "DOCS"}, DefaultVersions: map[string]string{"mysql": "10.11m", "ssl": "base"}}
	b := PackageOverlay{OptionsSet: []string{"DOCS", "NLS"}, DefaultVersions: map[string]string{"ssl": "base", "mysql": "10.11m"}}
	if got := a.Digest(); got != b.Digest() || len(got) != 64 || a.OptionsSet[0] != "NLS" {
		t.Fatal("digest depends on order or mutated the port options", got)
	}
	b.DefaultVersions["mysql"] = "8.4"
	if a.Digest() == b.Digest() {
		t.Fatal("changing a dependency must invalidate the build fingerprint")
	}
}

func TestExpandedOverlayConfigurationFitsChildSnapshot(t *testing.T) {
	option := strings.Repeat("X", MaxConfigBytes-100)
	raw := `{"package_overlays":{"devel/test":{"options_set":["` + option + `"]}}}`
	_, err := DecodeConfiguration([]byte(raw), "")
	if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "expanded") {
		t.Fatalf("accepted unusable child snapshot: %v", err)
	}
}
