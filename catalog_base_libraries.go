package poudrine

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func (c BuilderConfig) buildJailRoot() string {
	if !buildName.MatchString(c.Jail) || !filepath.IsAbs(c.BaseFS) {
		return ""
	}
	// Poudriere records the mount point supplied to jail -M here.
	if filepath.IsAbs(c.ConfigDir) {
		data, err := os.ReadFile(filepath.Join(c.ConfigDir, "poudriere.d", "jails", c.Jail, "mnt"))
		if !errors.Is(err, os.ErrNotExist) {
			path := strings.TrimSpace(string(data))
			if err != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path {
				return ""
			}
			return path
		}
	}
	return filepath.Join(c.BaseFS, "jails", c.Jail)
}

var baseLibraryName = regexp.MustCompile(`^.+\.so\.[0-9]+(\.[0-9]+)*$`)

// A ports repository omits libraries that pkg can load from the base system.
// Inspect the worker's target jail, including its optional compat32 libraries.
func jailBaseLibraries(root string) map[string]bool {
	if !filepath.IsAbs(root) {
		return nil
	}
	jail, err := os.OpenRoot(root)
	if err != nil {
		return nil
	}
	defer jail.Close()
	files := make(map[string]bool)
	aliases := make(map[string]string)
	usr, _ := jail.Lstat("usr")
	for _, dir := range []string{"lib", "usr/lib", "usr/lib32"} {
		if strings.HasPrefix(dir, "usr/") && (usr == nil || !usr.IsDir()) {
			continue
		}
		info, err := jail.Lstat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		entries, err := fs.ReadDir(jail.FS(), dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !baseLibraryName.MatchString(entry.Name()) {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			switch entry.Type() {
			case 0:
				files[path] = true
			case os.ModeSymlink:
				target, err := jail.Readlink(path)
				if err != nil || filepath.Clean(target) != target {
					continue
				}
				if filepath.IsAbs(target) {
					target = strings.TrimPrefix(target, "/")
				} else {
					target = filepath.Join(dir, target)
				}
				aliases[path] = target
			}
		}
	}
	// Resolve only aliases between inventoried base libraries of the same ABI.
	// An absolute symlink is relative to the jail's root on the client.
	for path, target := range aliases {
		for hops := 1; hops <= 40 && target != ""; hops++ {
			if files[target] && (filepath.Dir(path) == "usr/lib32") == (filepath.Dir(target) == "usr/lib32") {
				files[path] = true
				break
			}
			target = aliases[target]
		}
	}
	libraries := make(map[string]bool, len(files))
	for path := range files {
		name := filepath.Base(path)
		if filepath.Dir(path) == "usr/lib32" {
			name += ":32"
		}
		libraries[name] = true
	}
	return libraries
}
