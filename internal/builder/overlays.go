package builder

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/inkeliz/poudrine/internal/config"
)

// make.conf is read before the port Makefile, including during dependency
// discovery. Match its directory so flavors and subpackages inherit the same
// settings without exporting them to any of the port's dependencies.
func overlayMakeConf(overlays config.PackageOverlays) string {
	var out strings.Builder
	for _, origin := range slices.Sorted(maps.Keys(overlays)) {
		o := overlays[origin]
		fmt.Fprintf(&out, ".if \"${.CURDIR}\" == \"${PORTSDIR:U/usr/ports}/%s\"\n", origin)
		for _, key := range slices.Sorted(maps.Keys(o.DefaultVersions)) {
			fmt.Fprintf(&out, "DEFAULT_VERSIONS:= ${DEFAULT_VERSIONS:N%s=*} %s=%s\n", key, key, o.DefaultVersions[key])
		}
		for _, change := range []struct {
			names      []string
			set, clear string
		}{{o.OptionsSet, "WITH", "WITHOUT"}, {o.OptionsUnset, "WITHOUT", "WITH"}} {
			if len(change.names) == 0 {
				continue
			}
			names := slices.Clone(change.names)
			slices.Sort(names)
			fmt.Fprintf(&out, "%s:= ${%s:N%s}\n", change.clear, change.clear, strings.Join(names, ":N"))
			fmt.Fprintf(&out, "_POUDRINE_%s= %s\n", change.set, strings.Join(names, " "))
			fmt.Fprintf(&out, "%s+= ${_POUDRINE_%s}\n", change.set, change.set)
		}
		fmt.Fprintf(&out, "_POUDRINE_OVERLAY_DIGEST= %s\n.endif\n", o.Digest())
	}
	return out.String()
}

// bsd.port.mk includes this hook after the port has defined its options.
// The marker is a real package option. Poudriere compares the selected options
// both when reusing archives and when considering -b latest downloads. This
// covers changed and removed overlays without deriving package names from ports.
const overlayPortsMK = `.if defined(_POUDRINE_OVERLAY_DIGEST)
.for opt in ${_POUDRINE_WITH} ${_POUDRINE_WITHOUT}
.if empty(COMPLETE_OPTIONS_LIST:M${opt})
.error Poudrine overlay for ${PKGORIGIN}: unknown option ${opt}
.endif
.endfor
.for opt in ${_POUDRINE_WITH}
.if empty(PORT_OPTIONS:M${opt})
.error Poudrine overlay for ${PKGORIGIN}: could not enable ${opt}
.endif
.endfor
.for opt in ${_POUDRINE_WITHOUT}
.if !empty(PORT_OPTIONS:M${opt})
.error Poudrine overlay for ${PKGORIGIN}: could not disable ${opt}
.endif
.endfor
.if !defined(_POUDRINE_OVERLAY_APPLIED)
_POUDRINE_OVERLAY_APPLIED= yes
_POUDRINE_OPTION= POUDRINE_OVERLAY_${_POUDRINE_OVERLAY_DIGEST:tu}
ALL_OPTIONS+= ${_POUDRINE_OPTION}
PORT_OPTIONS+= ${_POUDRINE_OPTION}
PKG_NOTES+= ` + config.OverlayAnnotation + `
PKG_NOTE_` + config.OverlayAnnotation + `= ${_POUDRINE_OVERLAY_DIGEST}
.endif
.endif
`

func configureOverlayTree(c config.Builder) error {
	var makefile, settings string
	if len(c.PackageOverlays) != 0 {
		// Jail jobs may run as an unprivileged account even if the service
		// starts with umask 077. The null mount exposes this tree's modes.
		if err := makeBuilderSharedDir(filepath.Join(c.OverlayDir(), "Mk")); err != nil {
			return err
		}
		// bulk -R runs clean-restricted in each mounted overlay. This tree
		// supplies only build settings and contains no ports to clean.
		makefile = "clean-restricted:\n\t@:\n"
		settings = overlayPortsMK
	} else if err := writeWorkerConfig(filepath.Join(c.ConfigDir, ".poudrine-build-origins"), ""); err != nil {
		return err
	}
	if err := writeWorkerConfig(filepath.Join(c.OverlayDir(), "Makefile"), makefile); err != nil {
		return err
	}
	return writeWorkerConfig(filepath.Join(c.OverlayDir(), "Mk", "bsd.overlay.mk"), settings)
}

func (b *Builder) registerOverlay(ctx context.Context) error {
	c := b.Cfg
	// Poudriere mounts this entire directory into each jail. Refuse to
	// expose unexpected files or links alongside the generated makefiles.
	dir := c.OverlayDir()
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch path {
		case dir, filepath.Join(dir, "Mk"):
			if entry.IsDir() {
				return nil
			}
		case filepath.Join(dir, "Makefile"), filepath.Join(dir, "Mk", "bsd.overlay.mk"):
			if entry.Type().IsRegular() {
				return nil
			}
		}
		return fmt.Errorf("unexpected entry in Poudrine's generated ports overlay: %s", path)
	}); err != nil {
		return err
	}
	// Poudriere stores each registered tree's host mountpoint here. Never
	// adopt an unrelated tree if an administrator has used this name.
	mnt := filepath.Join(c.ConfigDir, "poudriere.d", "ports", config.OverlayTreeName, "mnt")
	path, err := readPackagesFile(mnt)
	if err == nil {
		if strings.TrimSpace(path) != dir {
			return fmt.Errorf("Poudriere tree %s is already registered at %s", config.OverlayTreeName, strings.TrimSpace(path))
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err = b.command(ctx, false, "ports", "-c", "-p", config.OverlayTreeName, "-m", "null", "-M", dir)
	return err
}
