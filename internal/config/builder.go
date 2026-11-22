package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A leading underscore keeps the internal native tree distinct from any
// user-configurable main tree, including after all overlays are removed.
const OverlayTreeName = "_poudrine_overlays"

func (c Builder) OverlayDir() string {
	return filepath.Join(c.ConfigDir, "poudriere.d", "poudrine-overlay")
}

func (c Builder) BuildListFile() string {
	if len(c.PackageOverlays) == 0 {
		return c.PackagesFile
	}
	return filepath.Join(c.ConfigDir, ".poudrine-build-origins")
}

// The worker runs separately from the HTTP servers. Its paths and configuration
// must be controlled by the administrator. Only origins come from the web DB.
type Builder struct {
	Release, Jail, Ports                 string
	ConfigDir, BaseFS, Distfiles         string
	SigningKey, PackagesFile, StatusFile string
	Jobs                                 int
	MakeJobsNumber, MakeJobsNumberLimit  int
	Interval                             time.Duration
	WorkingHours                         WorkingHours
	PackageOverlays                      PackageOverlays
}

func DefaultBuilder() Builder {
	return Builder{
		Jail: "latest", Ports: "main", ConfigDir: "/usr/local/etc/poudrine",
		BaseFS: "/usr/local/poudriere", Distfiles: "/var/cache/distfiles",
		SigningKey: "/usr/local/etc/poudriere.key", PackagesFile: "/usr/local/etc/poudriere.d/packages.txt",
		StatusFile: "/var/run/poudrine/build.json", Jobs: 6, Interval: 15 * time.Minute,
	}
}

func (c Builder) RepositoryDir() string {
	return filepath.Join(c.BaseFS, "data", "packages", c.Jail+"-"+c.Ports)
}

var BuildName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]*$`)

var buildPath = regexp.MustCompile(`^/[A-Za-z0-9/_.+-]+$`)

var buildRelease = regexp.MustCompile(`^([1-9][0-9]*)\.(0|[1-9][0-9]*)-RELEASE(-p(0|[1-9][0-9]*))?$`)

func (c Builder) Validate() error {
	if err := c.PackageOverlays.validate(); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{{"jail", c.Jail}, {"ports", c.Ports}} {
		if !BuildName.MatchString(field.value) {
			return fmt.Errorf("%s: start with a letter or digit and use only ASCII letters, digits or underscores", field.name)
		}
	}
	if c.Jobs < 1 || int64(c.Jobs) > 1<<53-1 {
		return errors.New("jobs: use a positive integer no greater than 9007199254740991")
	}
	for _, setting := range []struct {
		name  string
		value int
	}{{"make_jobs_number", c.MakeJobsNumber}, {"make_jobs_number_limit", c.MakeJobsNumberLimit}} {
		if setting.value < 0 || setting.value > 1<<31-1 {
			return fmt.Errorf("%s: use a whole number between 0 and 2147483647", setting.name)
		}
	}
	if c.Interval <= 0 {
		return errors.New("build_interval: use a positive duration")
	}
	if c.Release != "" {
		if _, _, err := JailRelease(c.Release); err != nil {
			return err
		}
	}
	paths := c.workerPaths()
	for _, p := range paths {
		if !buildPath.MatchString(p.path) || filepath.Clean(p.path) != p.path {
			return fmt.Errorf("%s: use a clean absolute path containing only letters, digits, /, _, ., + or -", p.field)
		}
	}
	return validatePathLayout(paths)
}

// Poudriere appends each plugin's make.conf to the build jail's make.conf.
// A dedicated fragment leaves administrator-owned port settings intact.
func (c Builder) MakeJobsFile() string {
	return filepath.Join(c.ConfigDir, "poudriere.d", "hooks", "plugins", "poudrine-jobs", "make.conf")
}

func JailRelease(value string) (int, string, error) {
	m := buildRelease.FindStringSubmatch(value)
	if m == nil {
		return 0, "", errors.New("release: use X.Y-RELEASE or X.Y-RELEASE-pN, or leave it empty for the host release")
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, "", errors.New("release: invalid major version")
	}
	return major, m[2], nil
}

func (c Builder) BuildJailRoot() string {
	if !BuildName.MatchString(c.Jail) || !filepath.IsAbs(c.BaseFS) {
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
