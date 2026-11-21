package poudrine

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

// passwd(5) defines the login-name characters; FreeBSD MAXLOGNAME includes the NUL.
func validateServiceUser(name string) error {
	if name == "_poudrine_status" {
		return errors.New("service_user: the public status account is reserved for the isolated reader")
	}
	if name == "" || name == "root" || name == "0" {
		return errors.New("service_user: select an unprivileged FreeBSD account")
	}
	if len(name) > 32 || name[0] == '-' || strings.ContainsAny(name, `,:+&#%^()!@~*?<>=|\/";`) || strings.Contains(strings.TrimSuffix(name, "$"), "$") {
		return errors.New("service_user: enter a valid FreeBSD login name of at most 32 bytes")
	}
	for _, b := range []byte(name) {
		if b < 0x21 || b > 0x7e {
			return errors.New("service_user: use a FreeBSD login name without spaces or control characters")
		}
	}
	return nil
}

type settingPath struct {
	field, path string
	directory   bool
}

func (p settingPath) checkLength() error {
	if len(p.path) >= unix.PathMax {
		return fmt.Errorf("%s: absolute path exceeds the system limit of %d bytes", p.field, unix.PathMax-1)
	}
	for part := range strings.SplitSeq(p.path, "/") {
		if len(part) > unix.NAME_MAX {
			return fmt.Errorf("%s: a path component exceeds %d bytes", p.field, unix.NAME_MAX)
		}
	}
	return nil
}

func (c BuilderConfig) workerPaths() []settingPath {
	return []settingPath{
		{"poudriere_config", c.ConfigDir, true},
		{"basefs", c.BaseFS, true},
		{"distfiles", c.Distfiles, true},
		{"poudriere_config", filepath.Join(c.ConfigDir, "poudriere.d"), true},
		{"poudriere_config", filepath.Join(c.ConfigDir, "worker.lock"), false},
		{"poudriere_config", filepath.Join(c.ConfigDir, ".poudrine-pauses"), false},
		{"poudriere_config", filepath.Join(c.ConfigDir, "poudriere.conf"), false},
		{"poudriere_config", c.makeJobsFile(), false},
		{"poudriere_config", filepath.Join(c.ConfigDir, ".poudrine-build-origins"), false},
		{"package_overlays", c.overlayDir(), true},
		{"package_overlays", filepath.Join(c.overlayDir(), "Makefile"), false},
		{"package_overlays", filepath.Join(c.overlayDir(), "Mk", "bsd.overlay.mk"), false},
		{"poudriere_config", filepath.Join(c.ConfigDir, "poudriere.d", "ports", overlayTreeName, "mnt"), false},
		{"basefs", c.RepositoryDir(), true},
		{"build_key", c.SigningKey, false},
		{"packages_file", c.PackagesFile, false},
		{"build_status", c.StatusFile, false},
	}
}

// The generated overlay tree is mounted into jails. Keep every other
// setting, including the signing and TLS keys, outside that tree.
func (p settingPath) conflictsWithChild(other settingPath) bool {
	return (!p.directory || p.field == "package_overlays" && other.field != p.field) && within(other.path, p.path)
}

// Distinct directory settings may normally be nested. A file must not
// also serve as a directory or be overwritten by another managed file.
func validatePathLayout(paths []settingPath) error {
	for i, p := range paths {
		if err := p.checkLength(); err != nil {
			return err
		}
		for _, other := range paths[:i] {
			if p.conflictsWithChild(other) || other.conflictsWithChild(p) {
				return fmt.Errorf("%s: path %q conflicts with %s at %q", p.field, p.path, other.field, other.path)
			}
		}
	}
	return nil
}

func (o *options) validatePaths() error {
	if o.tlsCert == "" && o.tlsKey != "" {
		return errors.New("tls_cert: supply the certificate that matches tls_key")
	}
	if o.tlsCert != "" && o.tlsKey == "" {
		return errors.New("tls_key: supply the private key that matches tls_cert")
	}
	workers := o.builder.workerPaths()
	all := workers
	for _, f := range []struct {
		field     string
		value     *string
		directory bool
	}{
		{"state", &o.state, true}, {"fingerprints", &o.fingerprints, true}, {"local_repo", &o.localRepo, true},
		{"tls_cert", &o.tlsCert, false}, {"tls_key", &o.tlsKey, false},
	} {
		p := *f.value
		if !f.directory && p == "" {
			continue // Automatic TLS certificate.
		}
		if p == "" || strings.TrimSpace(p) != p || strings.IndexFunc(p, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s: enter a path without surrounding whitespace or control characters", f.field)
		}
		if !f.directory && (strings.HasSuffix(p, "/") || filepath.Base(p) == "." || filepath.Base(p) == "..") {
			return fmt.Errorf("%s: enter a filename", f.field)
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(o.configPath), p)
		}
		path, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("%s: %w", f.field, err)
		}
		*f.value = path
		// OpenSSL can supply a certificate and matching key in a single PEM file.
		if f.field == "tls_key" && path == o.tlsCert {
			continue
		}
		all = append(all, settingPath{f.field, path, f.directory})
	}
	if o.state == "/" {
		return errors.New("state: select a private directory below the filesystem root")
	}
	for _, p := range workers {
		if within(p.path, o.state) {
			return fmt.Errorf("%s: privileged worker storage must be outside the private state directory", p.field)
		}
	}
	// An installation may keep its worker settings and web state on the same
	// volume. Keep the private data out of the trees Poudriere exposes to jails.
	for _, p := range []settingPath{
		{"distfiles", o.builder.Distfiles, true},
		{"basefs", filepath.Join(o.builder.BaseFS, "jails"), true},
		{"basefs", filepath.Join(o.builder.BaseFS, "ports"), true},
		{"basefs", filepath.Join(o.builder.BaseFS, "data"), true},
		{"poudriere_config", filepath.Join(o.builder.ConfigDir, "poudriere.d"), true},
	} {
		if within(o.state, p.path) {
			return fmt.Errorf("state: select a private directory outside %s (%s)", p.field, p.path)
		}
	}
	// The SQLite database, its journals and the signing identity have fixed
	// names under state. A manually located config can live beside them.
	var reserved []settingPath
	if o.configPath != "" {
		config, err := filepath.Abs(o.configPath)
		if err != nil {
			return err
		}
		for _, suffix := range []string{"", ".lock", ".nonces", ".admin.pub", ".setup-token"} {
			reserved = append(reserved, settingPath{"configuration file", config + suffix, false})
		}
	}
	reserved = append(reserved, settingPath{"state", filepath.Join(o.state, "cache"), true})
	liveArchives := filepath.Join(o.state, liveArchiveDirName)
	for _, name := range []string{"inventory.sqlite", "inventory.sqlite-wal", "inventory.sqlite-shm", "inventory.sqlite-journal", "service.lock", "repository.key", "repository.pub", "setup-token", standaloneStatsName} {
		reserved = append(reserved, settingPath{"state", filepath.Join(o.state, name), false})
	}
	if o.webTLS && o.tlsCert == "" {
		for _, name := range []string{"web-tls.crt", "web-tls.key"} {
			reserved = append(reserved, settingPath{"state", filepath.Join(o.state, name), false})
		}
	}

	paths := append(reserved, all...)
	for _, p := range paths {
		if within(p.path, liveArchives) {
			return fmt.Errorf("%s: %s is reserved for built package archives", p.field, liveArchives)
		}
		if within(p.path, publicStatsDir) {
			return fmt.Errorf("%s: %s is reserved for the public catalog snapshot", p.field, publicStatsDir)
		}
	}
	return validatePathLayout(paths)
}
