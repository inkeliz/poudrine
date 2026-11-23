package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"golang.org/x/sys/unix"
)

func rootControlled(path string, directory bool) (bool, error) {
	_, exists, err := InspectRootPath(path, directory, false)
	return exists, err
}

func CheckRootConfigPath(path string) error {
	_, err := rootControlled(path, false)
	return err
}

func Within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// Remote changes to privileged write locations are confined to dedicated
// storage. A host administrator may establish another root on disk through
// the configuration file. Web edits can then select locations below it.
func CheckBrokerSettings(ctx context.Context, configPath string, old, next Configuration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := old.Options(configPath)
	if err != nil {
		return err
	}
	after, err := next.Options(configPath)
	if err != nil {
		return err
	}
	a, b := before.Builder, after.Builder
	if before.ServiceUser != after.ServiceUser {
		if err := checkWebServiceUser(ctx, after.ServiceUser, before.State, after.State); err != nil {
			return fmt.Errorf("service_user: %w", err)
		}
	}
	type move struct {
		field, old, next, area string
		directory              bool
	}
	for _, m := range []move{
		{"poudriere_config", a.ConfigDir, b.ConfigDir, "/usr/local/etc/poudrine", true},
		{"basefs", a.BaseFS, b.BaseFS, "/usr/local/poudriere", true},
		{"distfiles", a.Distfiles, b.Distfiles, "/var/cache/distfiles", true},
		{"packages_file", a.PackagesFile, b.PackagesFile, "/usr/local/etc/poudriere.d", false},
		{"build_status", a.StatusFile, b.StatusFile, "/var/run/poudrine", false},
		{"build_key", a.SigningKey, b.SigningKey, "/usr/local/etc/poudrine/keys", false},
	} {
		if m.old == m.next {
			continue
		}
		base := m.old
		if !m.directory {
			base = filepath.Dir(m.old)
		}
		// /usr/local/etc (the default signing key's parent) is shared system
		// configuration. Other system parents are likewise never granted.
		if !Within(m.next, m.area) && !Within(m.next, "/var/db/poudrine-worker") && !mountedStorage(m.next) && !(dedicatedWorkerDir(base) && Within(m.next, base)) {
			return fmt.Errorf("%s: choose a worker location under %s, /var/db/poudrine-worker, /mnt, or /media", m.field, m.area)
		}
		if Within(m.next, after.State) || m.next == configPath || strings.HasPrefix(m.next, configPath+".") {
			return fmt.Errorf("%s: worker location %s overlaps service state", m.field, m.next)
		}
		exists, err := rootControlled(m.next, m.directory)
		if err != nil {
			return fmt.Errorf("%s: %w", m.field, err)
		}
		if exists {
			if !m.directory {
				return fmt.Errorf("%s: choose a new worker file (%s already exists)", m.field, m.next)
			}
			children, err := os.ReadDir(m.next)
			if err != nil {
				return fmt.Errorf("%s: %w", m.field, err)
			}
			if len(children) > 0 {
				return fmt.Errorf("%s: choose an empty worker directory (%s is in use)", m.field, m.next)
			}
		}
	}
	// Never let a generated data file become a shell hook or a file in a build jail.
	for _, f := range []struct{ field, old, path, extension string }{
		{"build_status", a.StatusFile, b.StatusFile, ".json"},
		{"packages_file", a.PackagesFile, b.PackagesFile, ".txt"},
		{"build_key", a.SigningKey, b.SigningKey, ".key"},
	} {
		if f.old != f.path && (filepath.Ext(f.path) != f.extension || Within(f.path, b.BaseFS) || Within(f.path, filepath.Join(b.ConfigDir, "poudriere.d", "hooks"))) {
			return fmt.Errorf("%s: choose a %s file outside the build jails and shell hooks", f.field, f.extension)
		}
	}
	if before.State != after.State {
		parent := filepath.Dir(after.State)
		if _, err := rootControlled(parent, true); err != nil {
			return fmt.Errorf("state: %w", err)
		}
		if !Within(after.State, "/var/db/poudrine-states") && !mountedStorage(after.State) && !(parent == filepath.Dir(before.State) && strings.HasPrefix(filepath.Base(after.State), "poudrine-")) {
			return errors.New("state: choose a new state directory under /mnt, /media, /var/db/poudrine-states or a poudrine-* sibling of the active state directory")
		}
		if s, err := os.Lstat(after.State); err == nil {
			st, ok := s.Sys().(*syscall.Stat_t)
			account, err := user.Lookup(after.ServiceUser)
			if err != nil || !ok || st == nil || !s.IsDir() || s.Mode().Perm() != 0700 || strconv.FormatUint(uint64(st.Uid), 10) != account.Uid {
				return errors.New("state: an existing web state directory must be private and owned by the selected service account")
			}
			entries, err := os.ReadDir(after.State)
			if err != nil || len(entries) != 0 {
				return errors.New("state: choose an empty or new state directory, or configure existing state on the host")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("state: %w", err)
		}
		if Within(configPath, after.State) {
			return errors.New("state: the web state directory cannot contain the service configuration")
		}
		for _, path := range []string{b.ConfigDir, b.BaseFS, b.Distfiles, b.SigningKey, b.PackagesFile, b.StatusFile} {
			if Within(path, after.State) || Within(after.State, path) {
				return errors.New("state: web state and privileged worker storage must be separate")
			}
		}
	}
	return PreflightConfiguration(ctx, "run", after)
}

func mountedStorage(path string) bool {
	return strings.HasPrefix(path, "/mnt/") || strings.HasPrefix(path, "/media/")
}

func dedicatedWorkerDir(dir string) bool {
	for _, part := range strings.Split(dir, "/") {
		if part == "poudrine" || part == "poudriere" || strings.HasPrefix(part, "poudrine-") || strings.HasPrefix(part, "poudriere.") {
			return true
		}
	}
	return false
}

func checkWebServiceUser(ctx context.Context, name, oldState, nextState string) error {
	// Use the FreeBSD account database (including its configured login shell).
	entry, err := exec.CommandContext(ctx, "/usr/sbin/pw", "usershow", "-n", name, "-7").Output()
	if err != nil {
		return errors.New("select an existing unprivileged service account")
	}
	fields := strings.Split(strings.TrimSpace(string(entry)), ":")
	if len(fields) != 7 || fields[0] != name || fields[2] == "0" || fields[3] == "0" || (fields[6] != "/usr/sbin/nologin" && fields[6] != "/bin/false") {
		return errors.New("the web service account must use nologin or false with a nonzero uid and primary gid")
	}
	uid, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return errors.New("invalid service account uid")
	}
	if oldState == nextState {
		if _, err := rootControlled(filepath.Dir(oldState), true); err != nil {
			return err
		}
		info, err := os.Lstat(oldState)
		var st *syscall.Stat_t
		if err == nil {
			st, _ = info.Sys().(*syscall.Stat_t)
		}
		if err != nil || st == nil || !info.IsDir() || info.Mode().Perm() != 0700 || st.Uid != uint32(uid) {
			return errors.New("also select a new state directory for the new service user, or give the new account ownership of the existing private Poudrine state")
		}
		// A local administrator explicitly assigned this active state to the new account.
		return nil
	}
	if !strings.Contains(strings.ToLower(fields[4]), "poudrine") {
		return errors.New("choose a dedicated non-login account with Poudrine in its account description, or assign the existing private Poudrine state to that account")
	}
	return nil
}

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

func (c Builder) workerPaths() []settingPath {
	return []settingPath{
		{"poudriere_config", c.ConfigDir, true},
		{"basefs", c.BaseFS, true},
		{"distfiles", c.Distfiles, true},
		{"poudriere_config", filepath.Join(c.ConfigDir, "poudriere.d"), true},
		{"poudriere_config", filepath.Join(c.ConfigDir, "worker.lock"), false},
		{"poudriere_config", filepath.Join(c.ConfigDir, ".poudrine-pauses"), false},
		{"poudriere_config", filepath.Join(c.ConfigDir, "poudriere.conf"), false},
		{"poudriere_config", c.MakeJobsFile(), false},
		{"poudriere_config", filepath.Join(c.ConfigDir, ".poudrine-build-origins"), false},
		{"package_overlays", c.OverlayDir(), true},
		{"package_overlays", filepath.Join(c.OverlayDir(), "Makefile"), false},
		{"package_overlays", filepath.Join(c.OverlayDir(), "Mk", "bsd.overlay.mk"), false},
		{"poudriere_config", filepath.Join(c.ConfigDir, "poudriere.d", "ports", OverlayTreeName, "mnt"), false},
		{"basefs", c.RepositoryDir(), true},
		{"build_key", c.SigningKey, false},
		{"packages_file", c.PackagesFile, false},
		{"build_status", c.StatusFile, false},
	}
}

// The generated overlay tree is mounted into jails. Keep every other
// setting, including the signing and TLS keys, outside that tree.
func (p settingPath) conflictsWithChild(other settingPath) bool {
	return (!p.directory || p.field == "package_overlays" && other.field != p.field) && Within(other.path, p.path)
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

func (o *Options) validatePaths() error {
	if o.TLSCert == "" && o.TLSKey != "" {
		return errors.New("tls_cert: supply the certificate that matches tls_key")
	}
	if o.TLSCert != "" && o.TLSKey == "" {
		return errors.New("tls_key: supply the private key that matches tls_cert")
	}
	workers := o.Builder.workerPaths()
	all := workers
	for _, f := range []struct {
		field     string
		value     *string
		directory bool
	}{
		{"state", &o.State, true}, {"fingerprints", &o.Fingerprints, true}, {"local_repo", &o.LocalRepo, true},
		{"tls_cert", &o.TLSCert, false}, {"tls_key", &o.TLSKey, false},
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
			p = filepath.Join(filepath.Dir(o.ConfigPath), p)
		}
		path, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("%s: %w", f.field, err)
		}
		*f.value = path
		// OpenSSL can supply a certificate and matching key in a single PEM file.
		if f.field == "tls_key" && path == o.TLSCert {
			continue
		}
		all = append(all, settingPath{f.field, path, f.directory})
	}
	if o.State == "/" {
		return errors.New("state: select a private directory below the filesystem root")
	}
	for _, p := range workers {
		if Within(p.path, o.State) {
			return fmt.Errorf("%s: privileged worker storage must be outside the private state directory", p.field)
		}
	}
	// An installation may keep its worker settings and web state on the same
	// volume. Keep the private data out of the trees Poudriere exposes to jails.
	for _, p := range []settingPath{
		{"distfiles", o.Builder.Distfiles, true},
		{"basefs", filepath.Join(o.Builder.BaseFS, "jails"), true},
		{"basefs", filepath.Join(o.Builder.BaseFS, "ports"), true},
		{"basefs", filepath.Join(o.Builder.BaseFS, "data"), true},
		{"poudriere_config", filepath.Join(o.Builder.ConfigDir, "poudriere.d"), true},
	} {
		if Within(o.State, p.path) {
			return fmt.Errorf("state: select a private directory outside %s (%s)", p.field, p.path)
		}
	}
	// The SQLite database, its journals and the signing identity have fixed
	// names under state. A manually located config can live beside them.
	var reserved []settingPath
	if o.ConfigPath != "" {
		config, err := filepath.Abs(o.ConfigPath)
		if err != nil {
			return err
		}
		reserved = configPaths(config)
	}
	reserved = append(reserved, settingPath{"state", filepath.Join(o.State, "cache"), true})
	liveArchives := filepath.Join(o.State, LiveArchiveDirName)
	for _, name := range []string{"inventory.sqlite", "inventory.sqlite-wal", "inventory.sqlite-shm", "inventory.sqlite-journal", "service.lock", "repository.key", "repository.pub", "setup-token", StandaloneStatsName} {
		reserved = append(reserved, settingPath{"state", filepath.Join(o.State, name), false})
	}
	if o.WebTLS && o.TLSCert == "" {
		for _, name := range []string{"web-tls.crt", "web-tls.key"} {
			reserved = append(reserved, settingPath{"state", filepath.Join(o.State, name), false})
		}
	}

	paths := append(reserved, all...)
	for _, p := range paths {
		if Within(p.path, liveArchives) {
			return fmt.Errorf("%s: %s is reserved for built package archives", p.field, liveArchives)
		}
		if Within(p.path, PublicStatsDir) {
			return fmt.Errorf("%s: %s is reserved for the public catalog snapshot", p.field, PublicStatsDir)
		}
	}
	return validatePathLayout(paths)
}

const PublicStatsDir = "/var/run/poudrine-status"

const PublicStatsFile = PublicStatsDir + "/status.json"

const StandaloneStatsName = "public-status.json"

const LiveArchiveDirName = "live-packages"

// A host administrator may use a root-managed symlink for worker storage.
// Validate its parents and its target in filesystem traversal order. Changes
// from the web broker use only real directories and regular files.
func InspectRootPath(path string, directory, allowLinks bool) (string, bool, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", false, fmt.Errorf("expected a clean absolute path: %s", path)
	}
	current := "/"
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	links := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}
		parent := current
		current = filepath.Join(parent, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			// A missing component below a writable parent can be claimed
			// before the worker creates it, even in a sticky directory.
			if err := protectedDirectory(parent); err != nil {
				return "", false, err
			}
			if slices.Contains(parts, "..") {
				return "", false, fmt.Errorf("missing directory in symlink target: %s", current)
			}
			return filepath.Join(append([]string{current}, parts...)...), false, nil
		}
		if err != nil {
			return "", false, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st == nil || st.Uid != 0 {
			return "", false, fmt.Errorf("%s must be root-owned", current)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if !allowLinks {
				return "", false, fmt.Errorf("expected a real path at %s", current)
			}
			target, err := os.Readlink(current)
			if err != nil {
				return "", false, err
			}
			links++
			if links > 40 {
				return "", false, fmt.Errorf("%s: %w", path, syscall.ELOOP)
			}
			current = parent
			if filepath.IsAbs(target) {
				current = "/"
			}
			parts = append(strings.Split(target, "/"), parts...)
			continue
		}
		// A root-owned sticky ancestor such as /tmp is safe to traverse into
		// an existing root-owned entry. It cannot be a new object's parent.
		stickyAncestor := info.IsDir() && info.Mode()&os.ModeSticky != 0 && len(parts) > 0
		if info.Mode().Perm()&0022 != 0 && !stickyAncestor {
			return "", false, fmt.Errorf("%s must not be group- or world-writable", current)
		}
		if len(parts) > 0 && !info.IsDir() {
			return "", false, fmt.Errorf("expected a real directory at %s", current)
		}
	}
	info, err := os.Stat(current)
	if err != nil {
		return "", false, err
	}
	if directory {
		if !info.IsDir() {
			return "", false, fmt.Errorf("expected a directory at %s", current)
		}
		if err := protectedDirectory(current); err != nil {
			return "", false, err
		}
	} else {
		if !info.Mode().IsRegular() {
			return "", false, fmt.Errorf("expected a regular file at %s", current)
		}
		if err := protectedDirectory(filepath.Dir(current)); err != nil {
			return "", false, err
		}
	}
	return current, true, nil
}

func protectedDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 || st.Uid != 0 {
		return fmt.Errorf("%s must be a root-owned directory without group or world write access", path)
	}
	return nil
}

func configPaths(path string) []settingPath {
	paths := []settingPath{{"configuration file", path, false}}
	for _, suffix := range CompanionSuffixes() {
		paths = append(paths, settingPath{"configuration file", path + suffix, false})
	}
	return paths
}

func (o Options) CheckWorkerStorage(mode string) error {
	var state string
	if mode != "worker" {
		parent, _, err := InspectRootPath(filepath.Dir(o.State), true, true)
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		state = filepath.Join(parent, filepath.Base(o.State))
	}
	paths := configPaths(o.ConfigPath)
	for _, p := range o.Builder.workerPaths() {
		// Poudriere manages the published repository (including symlinks)
		// below basefs. The worker writes directly to basefs and its metadata.
		if p.path == o.Builder.RepositoryDir() && p.field == "basefs" {
			continue
		}
		resolved, _, err := InspectRootPath(p.path, p.directory, true)
		if err != nil {
			return fmt.Errorf("%s: %w", p.field, err)
		}
		if state != "" && Within(resolved, state) {
			return fmt.Errorf("%s: worker storage resolves into the private state directory", p.field)
		}
		if Within(resolved, PublicStatsDir) {
			return fmt.Errorf("%s: worker storage resolves into the public stats directory", p.field)
		}
		paths = append(paths, settingPath{p.field, resolved, p.directory})
	}
	return validatePathLayout(paths)
}
