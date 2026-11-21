package poudrine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/inkeliz/poudrine/internal/requestauth"
	"golang.org/x/sys/unix"
)

// Only the supervisor can change the service configuration and its pinned admin
// identity. The web process forwards the signed original request across an
// inherited descriptor. No privileged service listens on the network.
type configAuthority struct {
	path    string
	builder *Builder
}

func configBrokerPair() (*os.File, *os.File, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(pair[0]), "config-supervisor"), os.NewFile(uintptr(pair[1]), "config-web"), nil
}

type configMessage struct {
	Method, Path, Time, Nonce, Signature, Key string
	Body                                      []byte
}

type configReply struct {
	Build          *BuildStatus   `json:",omitempty"`
	Origins        []string       `json:",omitzero"`
	PoudriereBytes *int64         `json:",omitempty"`
	MoreOrigins    bool           `json:",omitempty"`
	JobChunk       []BuildJob     `json:",omitempty"`
	AlreadyQueued  bool           `json:",omitempty"`
	Settings       *Configuration `json:",omitempty"`
	WorkerPending  bool           `json:"HoursPending,omitempty"` // Keep the wire name during an in-place service upgrade.
	ServerTime     string         `json:",omitempty"`
	Revision       string         `json:",omitempty"`
	PublicKey      []byte         `json:",omitempty"`
	Status         int
	Error          string `json:",omitempty"`
}

const configFrameLimit = 128 << 10

var errConfigFrameSize = errors.New("configuration message too large")

func writeConfigFrame(f *os.File, value any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return err
	}
	if buf.Len() > configFrameLimit {
		return errConfigFrameSize
	}
	_, err := f.Write(buf.Bytes())
	return err
}

// A whole ports tree can exceed one broker frame. Each validated origin
// occupies at most 255 ASCII bytes. Send chunks from the same file snapshot
// and hold the client's exchange lock until it has consumed the final frame.
const originsPerFrame = 256

func writeConfigReply(file *os.File, reply configReply) error {
	// Keep the entire snapshot, including logs, within the per-frame limit
	// even when many packages build concurrently. Job fields come from
	// Poudriere's size-limited status files; leave room for the final metadata.
	const jobsPerFrame = 24
	if reply.Build != nil && len(reply.Build.Jobs) > jobsPerFrame {
		build := *reply.Build
		for len(build.Jobs) > jobsPerFrame {
			if err := writeConfigFrame(file, configReply{JobChunk: build.Jobs[:jobsPerFrame]}); err != nil {
				return err
			}
			build.Jobs = build.Jobs[jobsPerFrame:]
		}
		reply.Build = &build
	}
	for len(reply.Origins) > originsPerFrame {
		chunk := configReply{Status: reply.Status, MoreOrigins: true, Origins: reply.Origins[:originsPerFrame]}
		if err := writeConfigFrame(file, chunk); err != nil {
			return err
		}
		reply.Origins = reply.Origins[originsPerFrame:]
	}
	return writeConfigFrame(file, reply)
}

type configClient struct {
	mu     sync.Mutex
	file   *os.File
	reader *bufio.Reader
}

func newConfigClient(file *os.File) *configClient {
	return &configClient{file: file, reader: bufio.NewReaderSize(file, configFrameLimit)}
}

func forwardedConfigRequest(r *http.Request, body []byte) configMessage {
	return configMessage{Method: r.Method, Path: r.URL.RequestURI(), Time: r.Header.Get("X-Poudrine-Time"), Nonce: r.Header.Get("X-Poudrine-Nonce"), Signature: r.Header.Get("X-Poudrine-Signature"), Key: r.Header.Get("X-Poudrine-Key"), Body: body}
}

func (c *configClient) exchange(m configMessage) (configReply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := writeConfigFrame(c.file, m); err != nil {
		return configReply{}, err
	}
	var origins []string
	var jobs []BuildJob
	for {
		data, err := c.reader.ReadSlice('\n')
		if err != nil {
			return configReply{}, err
		}
		var reply configReply
		if err := json.Unmarshal(data, &reply); err != nil {
			return configReply{}, err
		}
		if len(reply.JobChunk) != 0 {
			jobs = append(jobs, reply.JobChunk...)
			continue
		}
		if !reply.MoreOrigins {
			if reply.Build != nil && len(jobs) != 0 {
				reply.Build.Jobs = append(jobs, reply.Build.Jobs...)
			}
			if len(origins) != 0 {
				reply.Origins = append(origins, reply.Origins...)
			}
			return reply, nil
		}
		origins = append(origins, reply.Origins...)
	}
}

func (c *configClient) submit(r *http.Request, body []byte) (string, int, error) {
	v, err := c.exchange(forwardedConfigRequest(r, body))
	if err != nil {
		return "", 503, errors.New("configuration supervisor unavailable")
	}
	if v.Error != "" {
		return "", v.Status, errors.New(v.Error)
	}
	return v.Revision, v.Status, nil
}

func (c *configClient) read(r *http.Request, body []byte) (configReply, error) {
	v, err := c.exchange(forwardedConfigRequest(r, body))
	if err != nil {
		return configReply{Status: 503}, errors.New("configuration supervisor unavailable")
	}
	if v.Error != "" {
		return v, errors.New(v.Error)
	}
	if v.Settings == nil {
		return configReply{Status: 503}, errors.New("missing configuration response")
	}
	return v, nil
}

// A supervised web process authenticates against the root-owned pin.
func (c *configClient) publicKey() (ed25519.PublicKey, error) {
	v, err := c.exchange(configMessage{Method: "current-key"})
	if err != nil {
		return nil, err
	}
	if v.Status != 200 || (len(v.PublicKey) != 0 && len(v.PublicKey) != ed25519.PublicKeySize) {
		return nil, errors.New("supervisor administrator identity unavailable")
	}
	return ed25519.PublicKey(v.PublicKey), nil
}

func (a *configAuthority) serve(file *os.File) {
	defer file.Close()
	reader := bufio.NewReaderSize(file, configFrameLimit)
	for {
		data, err := reader.ReadSlice('\n')
		if err != nil {
			return
		}
		var m configMessage
		if json.Unmarshal(data, &m) != nil {
			return
		}
		if err := writeConfigReply(file, a.handle(m)); errors.Is(err, errConfigFrameSize) {
			if writeConfigFrame(file, configReply{Status: 413, Error: err.Error()}) != nil {
				return
			}
		} else if err != nil {
			return
		}
	}
}

// Publish a complete new identity or token exactly once, including when two
// starters or retries overlap. Existing files are never replaced.
func publishIdentity(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".poudrine-auth-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	err = os.Link(f.Name(), path)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

func newConfigAuthority(configPath string, migrated ed25519.PublicKey) (*configAuthority, error) {
	a := &configAuthority{path: configPath}
	for _, suffix := range []string{".admin.pub", ".setup-token", ".nonces", ".lock"} {
		if err := checkRootConfigPath(configPath + suffix); err != nil {
			return nil, err
		}
	}
	lock, err := lockConfiguration(a.path)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	key, err := a.publicKey()
	if err != nil {
		return nil, err
	}
	if key != nil {
		return a, nil
	}
	if _, err := os.Stat(configPath + ".setup-token"); err == nil {
		return a, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(migrated) > 0 {
		if len(migrated) != ed25519.PublicKeySize {
			return nil, errors.New("invalid previously enrolled administrator")
		}
		return a, publishIdentity(configPath+".admin.pub", migrated)
	}
	var secret [32]byte
	rand.Read(secret[:])
	return a, publishIdentity(configPath+".setup-token", []byte(base64.RawURLEncoding.EncodeToString(secret[:])+"\n"))
}

func (a *configAuthority) publicKey() (ed25519.PublicKey, error) {
	b, err := os.ReadFile(a.path + ".admin.pub")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid pinned administrator public key")
	}
	return ed25519.PublicKey(b), nil
}

func (m configMessage) signed(key ed25519.PublicKey) bool {
	_, _, ok := requestauth.Verify(key, m.Method, m.Path, m.Time, m.Nonce, m.Signature, m.Body)
	return ok
}

func (a *configAuthority) enroll(m configMessage, pin ed25519.PublicKey) configReply {
	key := requestauth.PublicKey(m.Key)
	if !m.signed(key) {
		return configReply{Status: 401, Error: "Invalid enrollment signature"}
	}
	// A lost response after pinning can be retried by the same signing key.
	if pin != nil {
		if bytes.Equal(pin, key) {
			return configReply{Status: 204}
		}
		return configReply{Status: 409, Error: "An administrator is already enrolled"}
	}
	expected, err := os.ReadFile(a.path + ".setup-token")
	if err != nil {
		return configReply{Status: 503, Error: "Administrator setup token unavailable"}
	}
	var body struct{ Token string }
	if json.Unmarshal(m.Body, &body) != nil || len(body.Token) != 43 || subtle.ConstantTimeCompare([]byte(body.Token), bytes.TrimSpace(expected)) != 1 {
		return configReply{Status: 403, Error: "Invalid setup token"}
	}
	if err := publishIdentity(a.path+".admin.pub", key); err != nil {
		return configReply{Status: 503, Error: "Cannot pin administrator identity"}
	}
	pin, err = a.publicKey()
	if err != nil || !bytes.Equal(pin, key) {
		return configReply{Status: 409, Error: "Another administrator has enrolled"}
	}
	_ = os.Remove(a.path + ".setup-token")
	return configReply{Status: 204}
}

func (a *configAuthority) handle(m configMessage) configReply {
	// Serialize authorization with enrollment and every privileged
	// write, even when another supervisor happens to use the same config.
	if m.Method == http.MethodPost || m.Method == http.MethodPut {
		lock, err := lockConfiguration(a.path)
		if err != nil {
			return configReply{Status: 503, Error: "Cannot lock service configuration"}
		}
		defer lock.Close()
	}
	pin, err := a.publicKey()
	if err != nil {
		return configReply{Status: 503, Error: "Cannot load administrator identity"}
	}
	if m.Method == "current-key" && m.Path == "" {
		return configReply{Status: 200, PublicKey: pin}
	}
	if m.Method == http.MethodPost && m.Path == "/api/setup" {
		return a.enroll(m, pin)
	}
	if !m.signed(pin) {
		return configReply{Status: 401, Error: "Invalid management request signature"}
	}
	if m.Method == methodQuery && !validQuery(m.Body, &struct{}{}) {
		return configReply{Status: 400, Error: "Expected an empty QUERY object"}
	}
	if m.Method == methodQuery && (m.Path == originsPath || m.Path == packagesPath) {
		reply := configReply{Status: http.StatusOK}
		if a.builder != nil {
			cfg := a.builder.cfg
			reply.Origins = cfg.PackageOverlays.buildOrigins(configuredOrigins(cfg.PackagesFile))
			if m.Path == packagesPath {
				reply.PoudriereBytes = a.builder.Status().PoudriereBytes
			}
		}
		return reply
	}
	if (m.Method == methodQuery && (m.Path == buildStatusPath || m.Path == buildEventsPath)) || (m.Method == http.MethodPost && m.Path == forceBuildPath) {
		return a.handleBuild(m)
	}
	if m.Method == http.MethodPost && m.Path == retryBuildPath {
		return a.handleBuildRetry(m)
	}
	if m.Path != "/api/admin/config" {
		return configReply{Status: 404, Error: "Unknown management operation"}
	}
	if m.Method != methodQuery && m.Method != http.MethodPut {
		return configReply{Status: 405, Error: "Unsupported settings operation"}
	}
	current, rev, err := loadConfiguration(a.path)
	if err != nil {
		if errors.Is(err, ErrInvalidConfig) {
			return configReply{Status: http.StatusUnprocessableEntity, Error: err.Error() + ". Correct the file on the host and reload settings."}
		}
		return configReply{Status: 503, Error: "Cannot load service configuration"}
	}
	if m.Method == methodQuery {
		reply := configReply{Settings: &current, Revision: rev, Status: 200, ServerTime: time.Now().Format("15:04 MST")}
		if a.builder != nil {
			hours, _ := parseWorkingHours(current.WorkingHours) // loadConfiguration validated the schedule.
			reply.WorkerPending = a.builder.currentWorkingHours() != hours || !a.builder.cfg.PackageOverlays.equal(current.PackageOverlays)
		}
		return reply
	}
	var body struct {
		Revision string
		Settings json.RawMessage
	}
	if json.Unmarshal(m.Body, &body) != nil {
		return configReply{Status: 400, Error: "Invalid settings request"}
	}
	if body.Revision != rev {
		return configReply{Status: 409, Error: "Settings have changed. Reload before saving."}
	}
	next, err := decodeConfiguration(body.Settings, a.path)
	if err == nil {
		err = checkBrokerSettings(a.path, current, next)
	}
	if err != nil {
		return configReply{Status: 400, Error: err.Error()}
	}
	if reply := a.recordNonce(m); reply.Error != "" {
		return reply
	}
	rev, err = saveLockedConfiguration(a.path, rev, next)
	if errors.Is(err, ErrConfigConflict) {
		return configReply{Status: 409, Error: "Settings have changed. Reload before saving."}
	}
	if errors.Is(err, ErrInvalidConfig) {
		return configReply{Status: 400, Error: err.Error()}
	}
	if err != nil {
		return configReply{Status: 503, Error: "Cannot save service configuration"}
	}
	return configReply{Revision: rev, Status: 200}
}

var errUsedAdminNonce = errors.New("request already processed")

func (a *configAuthority) recordNonce(m configMessage) configReply {
	if err := a.consumeNonce(m); errors.Is(err, errUsedAdminNonce) {
		return configReply{Status: 409, Error: err.Error()}
	} else if err != nil {
		return configReply{Status: 503, Error: "Cannot persist administrator replay window"}
	}
	return configReply{}
}

// The supervisor keeps its own short replay window, including across restarts.
// A compromised web child cannot reset it by replacing the web database.
func (a *configAuthority) consumeNonce(m configMessage) error {
	path := a.path + ".nonces"
	data, err := os.ReadFile(path)
	seen := map[string]int64{}
	if err == nil {
		err = json.Unmarshal(data, &seen)
	}
	if seen == nil || err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot read administrator replay window")
	}
	now := time.Now().Unix()
	for n, expires := range seen {
		if expires < now {
			delete(seen, n)
		}
	}
	if _, ok := seen[m.Nonce]; ok {
		return errUsedAdminNonce
	}
	stamp, _ := strconv.ParseInt(m.Time, 10, 64)
	seen[m.Nonce] = stamp + requestauth.ClockSkewSeconds
	data, err = json.Marshal(seen)
	if err != nil {
		return err
	}
	return writeAtomicFile(path, data, 0600)
}

func rootControlled(path string, directory bool) (bool, error) {
	_, exists, err := inspectRootPath(path, directory, false)
	return exists, err
}

func checkRootConfigPath(path string) error {
	_, err := rootControlled(path, false)
	return err
}

func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// Remote changes to privileged write locations are confined to dedicated
// storage. A host administrator may establish another root on disk through
// the configuration file. Web edits can then select locations below it.
func checkBrokerSettings(configPath string, old, next Configuration) error {
	before, err := old.options(configPath)
	if err != nil {
		return err
	}
	after, err := next.options(configPath)
	if err != nil {
		return err
	}
	a, b := before.builder, after.builder
	if before.serviceUser != after.serviceUser {
		if err := checkWebServiceUser(after.serviceUser, before.state, after.state); err != nil {
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
		if !within(m.next, m.area) && !within(m.next, "/var/db/poudrine-worker") && !mountedStorage(m.next) && !(dedicatedWorkerDir(base) && within(m.next, base)) {
			return fmt.Errorf("%s: choose a worker location under %s, /var/db/poudrine-worker, /mnt, or /media", m.field, m.area)
		}
		if within(m.next, after.state) || m.next == configPath || strings.HasPrefix(m.next, configPath+".") {
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
		if f.old != f.path && (filepath.Ext(f.path) != f.extension || within(f.path, b.BaseFS) || within(f.path, filepath.Join(b.ConfigDir, "poudriere.d", "hooks"))) {
			return fmt.Errorf("%s: choose a %s file outside the build jails and shell hooks", f.field, f.extension)
		}
	}
	if before.state != after.state {
		parent := filepath.Dir(after.state)
		if _, err := rootControlled(parent, true); err != nil {
			return fmt.Errorf("state: %w", err)
		}
		if !within(after.state, "/var/db/poudrine-states") && !mountedStorage(after.state) && !(parent == filepath.Dir(before.state) && strings.HasPrefix(filepath.Base(after.state), "poudrine-")) {
			return errors.New("state: choose a new state directory under /mnt, /media, /var/db/poudrine-states or a poudrine-* sibling of the active state directory")
		}
		if s, err := os.Lstat(after.state); err == nil {
			account, err := user.Lookup(after.serviceUser)
			if err != nil || !s.IsDir() || s.Mode().Perm() != 0700 || strconv.FormatUint(uint64(s.Sys().(*syscall.Stat_t).Uid), 10) != account.Uid {
				return errors.New("state: an existing web state directory must be private and owned by the selected service account")
			}
			entries, err := os.ReadDir(after.state)
			if err != nil || len(entries) != 0 {
				return errors.New("state: choose an empty or new state directory, or configure existing state on the host")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("state: %w", err)
		}
		if within(configPath, after.state) {
			return errors.New("state: the web state directory cannot contain the service configuration")
		}
		for _, path := range []string{b.ConfigDir, b.BaseFS, b.Distfiles, b.SigningKey, b.PackagesFile, b.StatusFile} {
			if within(path, after.state) || within(after.state, path) {
				return errors.New("state: web state and privileged worker storage must be separate")
			}
		}
	}
	return preflightConfiguration(context.Background(), "run", after)
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

func checkWebServiceUser(name, oldState, nextState string) error {
	// Use the FreeBSD account database (including its configured login shell).
	entry, err := exec.Command("/usr/sbin/pw", "usershow", "-n", name, "-7").Output()
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
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(uid) {
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
