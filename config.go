package poudrine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const defaultConfigPath = "/usr/local/etc/poudrine.json"
const maxConfigBytes = 64 << 10

var ErrConfigConflict = errors.New("configuration changed; reload the current settings")
var ErrInvalidConfig = errors.New("invalid configuration")

// Configuration is the administrator's JSON document. Empty automatic settings
// are resolved only when starting a process, so saving a configuration does
// not accidentally pin a generated URL, ABI or derived repository directory.
type Configuration struct {
	WebTLS              bool    `json:"web_tls"`
	TLSCert             string  `json:"tls_cert"`
	TLSKey              string  `json:"tls_key"`
	RepoListen          string  `json:"repo_listen"`
	WebListen           string  `json:"web_listen"`
	RepoURL             string  `json:"repo_url"`
	WebURL              string  `json:"web_url"`
	State               string  `json:"state"`
	ServiceUser         string  `json:"service_user"`
	Upstream            string  `json:"upstream"`
	Fingerprints        string  `json:"fingerprints"`
	LocalRepo           string  `json:"local_repo"`
	TrustedProxy        string  `json:"trusted_proxy"`
	PushContact         string  `json:"push_contact"`
	CacheGB             float64 `json:"cache_gb"`
	Refresh             string  `json:"refresh"`
	Release             string  `json:"release"`
	Jail                string  `json:"jail"`
	Ports               string  `json:"ports"`
	PoudriereConfig     string  `json:"poudriere_config"`
	BaseFS              string  `json:"basefs"`
	Distfiles           string  `json:"distfiles"`
	BuildKey            string  `json:"build_key"`
	PackagesFile        string  `json:"packages_file"`
	BuildStatus         string  `json:"build_status"`
	Jobs                int     `json:"jobs"`
	MakeJobsNumber      int     `json:"make_jobs_number"`
	MakeJobsNumberLimit int     `json:"make_jobs_number_limit"`
	BuildInterval       string  `json:"build_interval"`
	WorkingHours        string  `json:"working_hours"`
}

func defaultConfiguration() Configuration {
	b := DefaultBuilderConfig()
	return Configuration{
		WebTLS:          true,
		RepoListen:      "[::]:8080",
		WebListen:       "[::]:8181",
		State:           "/var/db/poudrine",
		ServiceUser:     "_poudrine",
		Fingerprints:    "/usr/share/keys/pkg",
		CacheGB:         5,
		Refresh:         "5m",
		Release:         b.Release,
		Jail:            b.Jail,
		Ports:           b.Ports,
		PoudriereConfig: b.ConfigDir,
		BaseFS:          b.BaseFS,
		Distfiles:       b.Distfiles,
		BuildKey:        b.SigningKey,
		PackagesFile:    b.PackagesFile,
		BuildStatus:     b.StatusFile,
		Jobs:            b.Jobs,
		BuildInterval:   b.Interval.String(),
	}
}

func (c Configuration) options(path string) (options, error) {
	o := options{
		webTLS:         c.WebTLS,
		tlsCert:        c.TLSCert,
		tlsKey:         c.TLSKey,
		configPath:     path,
		configSettings: c,
		state:          c.State,
		serviceUser:    c.ServiceUser,
		fingerprints:   c.Fingerprints,
		localRepo:      c.LocalRepo,
		proxy:          c.TrustedProxy,
		cacheGB:        c.CacheGB,
	}
	var err error
	if o.publicURL, err = publicHTTPURL(c.RepoURL); err != nil {
		return o, fmt.Errorf("repo_url: %w", err)
	}
	if o.websiteURL, err = baseHTTPURL(c.WebURL, true); err != nil {
		return o, fmt.Errorf("web_url: %w", err)
	}
	if o.upstream, err = publicHTTPURL(c.Upstream); err != nil {
		return o, fmt.Errorf("upstream: %w", err)
	}
	if o.repoListen, err = listenAddress(c.RepoListen); err != nil {
		return o, fmt.Errorf("repo_listen: %w", err)
	}
	if o.webListen, err = listenAddress(c.WebListen); err != nil {
		return o, fmt.Errorf("web_listen: %w", err)
	}
	if listenersOverlap(o.repoListen, o.webListen) {
		return o, errors.New("web_listen: overlaps repo_listen; choose a different address or port")
	}
	if err := checkProxyCIDR(c.TrustedProxy); err != nil {
		return o, fmt.Errorf("trusted_proxy: %w", err)
	}
	if o.pushContact, err = pushSubscriber(c.PushContact); err != nil {
		return o, fmt.Errorf("push_contact: %w", err)
	}
	if err := validateServiceUser(c.ServiceUser); err != nil {
		return o, err
	}
	if c.CacheGB < 0 || math.IsNaN(c.CacheGB) || c.CacheGB*1e9 >= float64(math.MaxInt64) {
		return o, errors.New("cache_gb: enter a nonnegative size that fits in a signed 64-bit byte count")
	}
	if c.CacheGB > 0 && c.CacheGB*1e9 < 1 {
		return o, errors.New("cache_gb: use zero to disable caching or set a limit of at least one byte")
	}
	o.refresh, err = time.ParseDuration(c.Refresh)
	if err != nil || o.refresh <= 0 {
		return o, errors.New("refresh: enter a positive duration such as 5m")
	}
	interval, err := time.ParseDuration(c.BuildInterval)
	if err != nil || interval <= 0 {
		return o, errors.New("build_interval: enter a positive duration such as 15m")
	}
	hours, err := parseWorkingHours(c.WorkingHours)
	if err != nil {
		return o, err
	}
	o.builder = BuilderConfig{
		Release:             c.Release,
		Jail:                c.Jail,
		Ports:               c.Ports,
		ConfigDir:           c.PoudriereConfig,
		BaseFS:              c.BaseFS,
		Distfiles:           c.Distfiles,
		SigningKey:          c.BuildKey,
		PackagesFile:        c.PackagesFile,
		StatusFile:          c.BuildStatus,
		Jobs:                c.Jobs,
		MakeJobsNumber:      c.MakeJobsNumber,
		MakeJobsNumberLimit: c.MakeJobsNumberLimit,
		Interval:            interval,
		WorkingHours:        hours,
	}
	if err := o.builder.validate(); err != nil {
		return o, err
	}
	if o.localRepo == "" {
		o.localRepo = o.builder.RepositoryDir()
	}
	if err := o.validatePaths(); err != nil {
		return o, err
	}
	return o, nil
}

// Use the JSON tags as the schema so adding a setting also makes it available
// to manual and signed web updates. Decode each member once, in source order.
// encoding/json struct decoding alone accepts duplicate names, case aliases
// and null scalars, all of which can silently change the operator's intent.
var configFields = func() map[string]int {
	t := reflect.TypeFor[Configuration]()
	fields := make(map[string]int, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		fields[t.Field(i).Tag.Get("json")] = i
	}
	return fields
}()

func decodeConfiguration(raw []byte, path string) (Configuration, error) {
	c := defaultConfiguration()
	if len(raw) > maxConfigBytes {
		return c, fmt.Errorf("%w: file exceeds %d bytes", ErrInvalidConfig, maxConfigBytes)
	}
	if !utf8.Valid(raw) {
		return c, fmt.Errorf("%w: expected UTF-8 JSON", ErrInvalidConfig)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	opening, err := d.Token()
	if err != nil || opening != json.Delim('{') {
		return c, fmt.Errorf("%w: expected a JSON object", ErrInvalidConfig)
	}
	fields := reflect.ValueOf(&c).Elem()
	seen := make([]bool, fields.NumField())
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return c, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
		}
		name, _ := token.(string)
		index, known := configFields[name]
		if !known {
			return c, fmt.Errorf("%w: unknown setting %q", ErrInvalidConfig, name)
		}
		if seen[index] {
			return c, fmt.Errorf("%w: %s: duplicate setting", ErrInvalidConfig, name)
		}
		seen[index] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return c, fmt.Errorf("%w: %s: %v", ErrInvalidConfig, name, err)
		}
		if bytes.Equal(value, []byte("null")) {
			return c, fmt.Errorf("%w: %s: omit the setting to use its default, or supply a value", ErrInvalidConfig, name)
		}
		field := fields.Field(index)
		if err := json.Unmarshal(value, field.Addr().Interface()); err != nil {
			return c, fmt.Errorf("%w: %s: %v", ErrInvalidConfig, name, err)
		}
		if field.Kind() == reflect.String && strings.ContainsRune(field.String(), utf8.RuneError) && !validSurrogatePairs(value) {
			return c, fmt.Errorf("%w: %s: invalid Unicode escape pair", ErrInvalidConfig, name)
		}
		// ParseFloat rounds extremely small values to zero. Zero has its own
		// meaning for cache_gb, so require a zero mantissa for that case.
		if name == "cache_gb" && c.CacheGB == 0 {
			mantissa, _, _ := strings.Cut(strings.ToLower(string(value)), "e")
			if strings.ContainsAny(mantissa, "123456789") {
				return c, fmt.Errorf("%w: cache_gb: use zero or at least one byte", ErrInvalidConfig)
			}
		}
	}
	if _, err := d.Token(); err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return c, fmt.Errorf("%w: trailing data", ErrInvalidConfig)
	}
	if _, err := c.options(path); err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return c, nil
}

// encoding/json accepts unpaired UTF-16 escapes and substitutes U+FFFD.
// Distinguish a real replacement character in a filename from a lossy decode.
// The caller has already checked JSON syntax, including every escape's length.
func validSurrogatePairs(raw []byte) bool {
	for {
		i := bytes.IndexByte(raw, '\\')
		if i < 0 {
			return true
		}
		raw = raw[i+1:]
		if raw[0] != 'u' {
			raw = raw[1:]
			continue
		}
		code, _ := strconv.ParseUint(string(raw[1:5]), 16, 16)
		raw = raw[5:]
		if !utf16.IsSurrogate(rune(code)) {
			continue
		}
		if len(raw) < 6 || raw[0] != '\\' || raw[1] != 'u' {
			return false
		}
		low, _ := strconv.ParseUint(string(raw[2:6]), 16, 16)
		if utf16.DecodeRune(rune(code), rune(low)) == utf8.RuneError {
			return false
		}
		raw = raw[6:]
	}
}

func configRevision(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// loadConfiguration is read-only, including when the file is missing.
func loadConfiguration(path string) (Configuration, string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Configuration{}, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Configuration{}, "", err
	}
	if !info.Mode().IsRegular() {
		return Configuration{}, "", fmt.Errorf("%s: configuration must be a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return Configuration{}, "", err
	}
	c, err := decodeConfiguration(data, path)
	if err != nil {
		return c, "", fmt.Errorf("%s: %w", path, err)
	}
	return c, configRevision(data), nil
}

// The lock has a stable inode across atomic replacements of the JSON file.
func lockConfiguration(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func saveConfiguration(path, revision string, raw json.RawMessage) (string, error) {
	c, err := decodeConfiguration(raw, path)
	if err != nil {
		return "", err
	}
	if c.WebTLS && c.TLSCert != "" {
		// Relative certificate paths use the JSON file's directory.
		o, err := c.options(path)
		if err != nil {
			return "", err
		}
		if _, err := loadWebsiteKeyPair(o.tlsCert, o.tlsKey); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidConfig, err)
		}
	}
	lock, err := lockConfiguration(path)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	return saveLockedConfiguration(path, revision, c)
}

func saveLockedConfiguration(path, revision string, c Configuration) (string, error) {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if len(data) > maxConfigBytes {
		return "", fmt.Errorf("%w: expanded file exceeds %d bytes", ErrInvalidConfig, maxConfigBytes)
	}
	_, current, err := loadConfiguration(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if revision != current {
		return "", ErrConfigConflict
	}
	mode := os.FileMode(0644)
	if st, err := os.Lstat(path); err == nil {
		if !st.Mode().IsRegular() {
			return "", errors.New("configuration must be a regular file")
		}
		mode = st.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := writeAtomicFile(path, data, mode); err != nil {
		return "", err
	}
	return configRevision(data), nil
}

func parseOptions(mode string, args []string) (options, error) {
	flags := flag.NewFlagSet("poudrine "+mode, flag.ContinueOnError)
	path := flags.String("config", defaultConfigPath, "JSON configuration file")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: poudrine [run|serve|worker|check-config|client-setup] [-config path]\n       poudrine set-tls -help\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	absolute, err := filepath.Abs(*path)
	if err != nil {
		return options{}, err
	}
	if os.Geteuid() == 0 && (mode == "run" || mode == "worker") {
		if err := checkRootConfigPath(absolute); err != nil {
			return options{}, err
		}
	}
	c, revision, err := loadConfiguration(absolute)
	if errors.Is(err, os.ErrNotExist) && (mode == "run" || mode == "serve" || mode == "worker") {
		// A second process may initialize the same file while we wait for the lock.
		if _, err = saveConfiguration(absolute, "", json.RawMessage("{}")); err == nil || errors.Is(err, ErrConfigConflict) {
			c, revision, err = loadConfiguration(absolute)
		}
	}
	if err != nil {
		return options{}, err
	}
	o, err := c.options(absolute)
	o.configRevision = revision
	return o, err
}
