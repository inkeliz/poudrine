package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/inkeliz/poudrine/internal/constanttime"
	"github.com/inkeliz/poudrine/internal/packageinfo"
)

// PackageOverlays applies build settings to all flavors and subpackages of an origin.
type PackageOverlays map[string]PackageOverlay

func (overlays PackageOverlays) BuildOrigins(origins []string) []string {
	if len(overlays) == 0 {
		return origins
	}
	all := slices.AppendSeq(slices.Clone(origins), maps.Keys(overlays))
	slices.Sort(all)
	return slices.Compact(all)
}

type PackageOverlay struct {
	OptionsSet      []string          `json:"options_set,omitempty"`
	OptionsUnset    []string          `json:"options_unset,omitempty"`
	DefaultVersions map[string]string `json:"default_versions,omitempty"`
}

const OverlayAnnotation = "poudrine_overlay"

// digest is recorded in built packages so an ordinary build of the same version
// or an archive built with an earlier overlay cannot satisfy this configuration.
func (o PackageOverlay) Digest() string {
	o.OptionsSet = slices.Clone(o.OptionsSet)
	o.OptionsUnset = slices.Clone(o.OptionsUnset)
	slices.Sort(o.OptionsSet)
	slices.Sort(o.OptionsUnset)
	data, _ := json.Marshal(o) // JSON sorts the keys of DefaultVersions.
	sum := sha256.Sum256(data)
	return constanttime.EncodeHex(sum[:])
}

func (overlays PackageOverlays) Equal(other PackageOverlays) bool {
	return maps.EqualFunc(overlays, other, func(a, b PackageOverlay) bool { return a.Digest() == b.Digest() })
}

var (
	overlayOption      = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_]*$`)
	overlayVersionName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	overlayVersion     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]*$`)
)

func (overlays PackageOverlays) validate() error {
	for origin, overlay := range overlays {
		if len(origin) > 255 || !packageinfo.BuildOrigin.MatchString(origin) || packageinfo.BasePort(origin) != origin {
			return fmt.Errorf("package_overlays: %q must be a category/port origin without a flavor or subpackage", origin)
		}
		seen := make(map[string]bool)
		for _, options := range [][]string{overlay.OptionsSet, overlay.OptionsUnset} {
			for _, name := range options {
				if !overlayOption.MatchString(name) {
					return fmt.Errorf("package_overlays: %s: invalid port option %q (use uppercase option names)", origin, name)
				}
				if seen[name] {
					return fmt.Errorf("package_overlays: %s: list option %q only once", origin, name)
				}
				seen[name] = true
			}
		}
		for name, version := range overlay.DefaultVersions {
			if !overlayVersionName.MatchString(name) {
				return fmt.Errorf("package_overlays: %s: invalid DEFAULT_VERSIONS name %q (use lowercase names)", origin, name)
			}
			// llvm=-devel is a ports framework choice with a leading hyphen.
			if !overlayVersion.MatchString(version) && !(name == "llvm" && version == "-devel") {
				return fmt.Errorf("package_overlays: %s: invalid DEFAULT_VERSIONS value %q for %s", origin, version, name)
			}
		}
	}
	return nil
}

// Nested objects follow the same strict member rules as the top-level configuration.
// A map or struct decoded directly by encoding/json would silently accept duplicates.
func overlayMembers(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	if opening, err := d.Token(); err != nil || opening != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	members := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		key := token.(string)
		if _, exists := members[key]; exists {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, err
		}
		if bytes.Equal(raw, []byte("null")) {
			return nil, fmt.Errorf("%s requires a value", key)
		}
		members[key] = raw
	}
	_, err := d.Token()
	return members, err
}

func decodeOverlayVersions(raw []byte) (map[string]string, error) {
	members, err := overlayMembers(raw)
	if err != nil {
		return nil, err
	}
	versions := make(map[string]string, len(members))
	for name, raw := range members {
		var version string
		if err := json.Unmarshal(raw, &version); err != nil {
			return nil, fmt.Errorf("%s: expected a version string", name)
		}
		versions[name] = version
	}
	return versions, nil
}

func (overlays *PackageOverlays) UnmarshalJSON(data []byte) error {
	origins, err := overlayMembers(data)
	if err != nil {
		return err
	}
	result := make(PackageOverlays, len(origins))
	for origin, raw := range origins {
		fields, err := overlayMembers(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", origin, err)
		}
		var overlay PackageOverlay
		for key, raw := range fields {
			switch key {
			case "options_set":
				err = json.Unmarshal(raw, &overlay.OptionsSet)
			case "options_unset":
				err = json.Unmarshal(raw, &overlay.OptionsUnset)
			case "default_versions":
				overlay.DefaultVersions, err = decodeOverlayVersions(raw)
			default:
				return fmt.Errorf("%s: unknown overlay setting %q", origin, key)
			}
			if err != nil {
				return fmt.Errorf("%s: %s: %w", origin, key, err)
			}
		}
		result[origin] = overlay
	}
	*overlays = result
	return nil
}
