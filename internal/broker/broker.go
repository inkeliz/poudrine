package broker

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
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	builderpkg "github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/fsutil"
	"github.com/inkeliz/poudrine/internal/requestauth"
	"github.com/inkeliz/poudrine/internal/routes"
	"golang.org/x/sys/unix"
)

// Only the supervisor can change the service configuration and its pinned admin
// identity. The web process forwards the signed original request across an
// inherited descriptor. No privileged service listens on the network.
type Authority struct {
	Path    string              `json:"-"`
	Builder *builderpkg.Builder `json:"-"`
}

func Pair() (*os.File, *os.File, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(pair[0]), "config-supervisor"), os.NewFile(uintptr(pair[1]), "config-web"), nil
}

type Message struct {
	Method, Path, Time, Nonce, Signature, Key string
	Body                                      []byte
	Deadline                                  time.Time `json:",omitzero"`
}

type Reply struct {
	Build          *builderpkg.BuildStatus `json:",omitempty"`
	Origins        []string                `json:",omitzero"`
	PoudriereBytes *int64                  `json:",omitempty"`
	MoreOrigins    bool                    `json:",omitempty"`
	JobChunk       []builderpkg.BuildJob   `json:",omitempty"`
	AlreadyQueued  bool                    `json:",omitempty"`
	Settings       *config.Configuration   `json:",omitempty"`
	WorkerPending  bool                    `json:"HoursPending,omitempty"` // Keep the wire name during an in-place service upgrade.
	ServerTime     string                  `json:",omitempty"`
	Revision       string                  `json:",omitempty"`
	PublicKey      []byte                  `json:",omitempty"`
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
const OriginsPerFrame = 256

func writeConfigReply(file *os.File, reply Reply) error {
	// Keep the entire snapshot, including logs, within the per-frame limit
	// even when many packages build concurrently. Job fields come from
	// Poudriere's size-limited status files; leave room for the final metadata.
	const jobsPerFrame = 24
	if reply.Build != nil && len(reply.Build.Jobs) > jobsPerFrame {
		build := *reply.Build
		for len(build.Jobs) > jobsPerFrame {
			if err := writeConfigFrame(file, Reply{JobChunk: build.Jobs[:jobsPerFrame]}); err != nil {
				return err
			}
			build.Jobs = build.Jobs[jobsPerFrame:]
		}
		reply.Build = &build
	}
	for len(reply.Origins) > OriginsPerFrame {
		chunk := Reply{Status: reply.Status, MoreOrigins: true, Origins: reply.Origins[:OriginsPerFrame]}
		if err := writeConfigFrame(file, chunk); err != nil {
			return err
		}
		reply.Origins = reply.Origins[OriginsPerFrame:]
	}
	return writeConfigFrame(file, reply)
}

type Client struct {
	mu     sync.Mutex
	file   *os.File
	reader *bufio.Reader
}

func NewClient(file *os.File) *Client {
	return &Client{file: file, reader: bufio.NewReaderSize(file, configFrameLimit)}
}

func ForwardedConfigRequest(r *http.Request, body []byte) Message {
	deadline, _ := r.Context().Deadline()
	return Message{Deadline: deadline, Method: r.Method, Path: r.URL.RequestURI(), Time: r.Header.Get("X-Poudrine-Time"), Nonce: r.Header.Get("X-Poudrine-Nonce"), Signature: r.Header.Get("X-Poudrine-Signature"), Key: r.Header.Get("X-Poudrine-Key"), Body: body}
}

func (c *Client) Exchange(m Message) (Reply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := writeConfigFrame(c.file, m); err != nil {
		return Reply{}, err
	}
	var origins []string
	var jobs []builderpkg.BuildJob
	for {
		data, err := c.reader.ReadSlice('\n')
		if err != nil {
			return Reply{}, err
		}
		var reply Reply
		if err := json.Unmarshal(data, &reply); err != nil {
			return Reply{}, err
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

func (c *Client) Submit(r *http.Request, body []byte) (string, int, error) {
	if err := r.Context().Err(); err != nil {
		return "", http.StatusServiceUnavailable, err
	}
	v, err := c.Exchange(ForwardedConfigRequest(r, body))
	if err != nil {
		return "", 503, errors.New("configuration supervisor unavailable")
	}
	if v.Error != "" {
		return "", v.Status, errors.New(v.Error)
	}
	return v.Revision, v.Status, nil
}

func (c *Client) Read(r *http.Request, body []byte) (Reply, error) {
	v, err := c.Exchange(ForwardedConfigRequest(r, body))
	if err != nil {
		return Reply{Status: 503}, errors.New("configuration supervisor unavailable")
	}
	if v.Error != "" {
		return v, errors.New(v.Error)
	}
	if v.Settings == nil {
		return Reply{Status: 503}, errors.New("missing configuration response")
	}
	return v, nil
}

// A supervised web process authenticates against the root-owned pin.
func (c *Client) PublicKey() (ed25519.PublicKey, error) {
	v, err := c.Exchange(Message{Method: "current-key"})
	if err != nil {
		return nil, err
	}
	if v.Status != 200 || (len(v.PublicKey) != 0 && len(v.PublicKey) != ed25519.PublicKeySize) {
		return nil, errors.New("supervisor administrator identity unavailable")
	}
	return ed25519.PublicKey(v.PublicKey), nil
}

func (a *Authority) Serve(ctx context.Context, file *os.File) {
	defer file.Close()
	reader := bufio.NewReaderSize(file, configFrameLimit)
	for {
		data, err := reader.ReadSlice('\n')
		if err != nil {
			return
		}
		var m Message
		if json.Unmarshal(data, &m) != nil {
			return
		}
		if err := writeConfigReply(file, a.Handle(ctx, m)); errors.Is(err, errConfigFrameSize) {
			if writeConfigFrame(file, Reply{Status: 413, Error: err.Error()}) != nil {
				return
			}
		} else if err != nil {
			return
		}
	}
}

func NewAuthority(configPath string, migrated ed25519.PublicKey) (*Authority, error) {
	a := &Authority{Path: configPath}
	for _, suffix := range config.CompanionSuffixes() {
		if err := config.CheckRootConfigPath(configPath + suffix); err != nil {
			return nil, err
		}
	}
	lock, err := config.LockConfiguration(a.Path)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	key, err := a.PublicKey()
	if err != nil {
		return nil, err
	}
	if key != nil {
		return a, nil
	}
	if _, err := os.Stat(configPath + config.SetupTokenSuffix); err == nil {
		return a, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(migrated) > 0 {
		if len(migrated) != ed25519.PublicKeySize {
			return nil, errors.New("invalid previously enrolled administrator")
		}
		return a, fsutil.CreateFileOnce(configPath+config.AdminKeySuffix, migrated, 0600)
	}
	var secret [32]byte
	rand.Read(secret[:])
	return a, fsutil.CreateFileOnce(configPath+config.SetupTokenSuffix, []byte(base64.RawURLEncoding.EncodeToString(secret[:])+"\n"), 0600)
}

func (a *Authority) PublicKey() (ed25519.PublicKey, error) {
	b, err := os.ReadFile(a.Path + config.AdminKeySuffix)
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

func (m Message) Signed(key ed25519.PublicKey) bool {
	_, _, ok := requestauth.Verify(key, m.Method, m.Path, m.Time, m.Nonce, m.Signature, m.Body)
	return ok
}

func (a *Authority) enroll(m Message, pin ed25519.PublicKey) Reply {
	key := requestauth.PublicKey(m.Key)
	if !m.Signed(key) {
		return Reply{Status: 401, Error: "Invalid enrollment signature"}
	}
	// A lost response after pinning can be retried by the same signing key.
	if pin != nil {
		if bytes.Equal(pin, key) {
			return Reply{Status: 204}
		}
		return Reply{Status: 409, Error: "An administrator is already enrolled"}
	}
	expected, err := os.ReadFile(a.Path + config.SetupTokenSuffix)
	if err != nil {
		return Reply{Status: 503, Error: "Administrator setup token unavailable"}
	}
	var body struct{ Token string }
	if json.Unmarshal(m.Body, &body) != nil || len(body.Token) != 43 || subtle.ConstantTimeCompare([]byte(body.Token), bytes.TrimSpace(expected)) != 1 {
		return Reply{Status: 403, Error: "Invalid setup token"}
	}
	if err := fsutil.CreateFileOnce(a.Path+config.AdminKeySuffix, key, 0600); err != nil {
		return Reply{Status: 503, Error: "Cannot pin administrator identity"}
	}
	pin, err = a.PublicKey()
	if err != nil || !bytes.Equal(pin, key) {
		return Reply{Status: 409, Error: "Another administrator has enrolled"}
	}
	_ = os.Remove(a.Path + config.SetupTokenSuffix)
	return Reply{Status: 204}
}

func (a *Authority) Handle(ctx context.Context, m Message) Reply {
	// Serialize authorization with enrollment and every privileged
	// write, even when another supervisor happens to use the same config.
	if m.Method == http.MethodPost || m.Method == http.MethodPut {
		lock, err := config.LockConfiguration(a.Path)
		if err != nil {
			return Reply{Status: 503, Error: "Cannot lock service configuration"}
		}
		defer lock.Close()
	}
	pin, err := a.PublicKey()
	if err != nil {
		return Reply{Status: 503, Error: "Cannot load administrator identity"}
	}
	if m.Method == "current-key" && m.Path == "" {
		return Reply{Status: 200, PublicKey: pin}
	}
	if routes.Enroll.Matches(m.Method, m.Path) {
		return a.enroll(m, pin)
	}
	if !m.Signed(pin) {
		return Reply{Status: 401, Error: "Invalid management request signature"}
	}
	if m.Method == MethodQuery && !ValidQuery(m.Body, &struct{}{}) {
		return Reply{Status: 400, Error: "Expected an empty QUERY object"}
	}
	if routes.Origins.Matches(m.Method, m.Path) || routes.Packages.Matches(m.Method, m.Path) {
		reply := Reply{Status: http.StatusOK}
		if a.Builder != nil {
			cfg := a.Builder.Cfg
			reply.Origins = cfg.PackageOverlays.BuildOrigins(builderpkg.ConfiguredOrigins(cfg.PackagesFile))
			if m.Path == routes.Packages.Path {
				reply.PoudriereBytes = a.Builder.Status().PoudriereBytes
			}
		}
		return reply
	}
	if routes.BuildStatus.Matches(m.Method, m.Path) || routes.BuildEvents.Matches(m.Method, m.Path) || routes.ForceBuild.Matches(m.Method, m.Path) {
		return a.handleBuild(m)
	}
	if routes.RetryBuild.Matches(m.Method, m.Path) {
		return a.handleBuildRetry(ctx, m)
	}
	if m.Path != routes.ReadConfig.Path {
		return Reply{Status: 404, Error: "Unknown management operation"}
	}
	if m.Method != routes.ReadConfig.Method && m.Method != routes.SaveConfig.Method {
		return Reply{Status: 405, Error: "Unsupported settings operation"}
	}
	current, rev, err := config.LoadConfiguration(a.Path)
	if err != nil {
		if errors.Is(err, config.ErrInvalidConfig) {
			return Reply{Status: http.StatusUnprocessableEntity, Error: err.Error() + ". Correct the file on the host and reload settings."}
		}
		return Reply{Status: 503, Error: "Cannot load service configuration"}
	}
	if m.Method == MethodQuery {
		reply := Reply{Settings: &current, Revision: rev, Status: 200, ServerTime: time.Now().Format("15:04 MST")}
		if a.Builder != nil {
			hours, _ := config.ParseWorkingHours(current.WorkingHours) // loadConfiguration validated the schedule.
			reply.WorkerPending = a.Builder.CurrentWorkingHours() != hours || !a.Builder.Cfg.PackageOverlays.Equal(current.PackageOverlays)
		}
		return reply
	}
	var body struct {
		Revision string
		Settings json.RawMessage
	}
	if json.Unmarshal(m.Body, &body) != nil {
		return Reply{Status: 400, Error: "Invalid settings request"}
	}
	if body.Revision != rev {
		return Reply{Status: 409, Error: "Settings have changed. Reload before saving."}
	}
	// Framed supervisor requests outlive the originating HTTP handler. A
	// forwarded deadline can shorten the supervisor's own validation budget.
	deadline := time.Now().Add(10 * time.Second)
	if !m.Deadline.IsZero() && m.Deadline.Before(deadline) {
		deadline = m.Deadline
	}
	checkCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	next, err := config.DecodeConfiguration(body.Settings, a.Path)
	if err == nil {
		err = config.CheckBrokerSettings(checkCtx, a.Path, current, next)
	}
	if err := checkCtx.Err(); err != nil {
		return Reply{Status: http.StatusServiceUnavailable, Error: "Configuration preflight: " + err.Error()}
	}
	if err != nil {
		return Reply{Status: 400, Error: err.Error()}
	}
	if reply := a.recordNonce(m); reply.Error != "" {
		return reply
	}
	rev, err = config.SaveLockedConfiguration(a.Path, rev, next)
	if errors.Is(err, config.ErrConfigConflict) {
		return Reply{Status: 409, Error: "Settings have changed. Reload before saving."}
	}
	if errors.Is(err, config.ErrInvalidConfig) {
		return Reply{Status: 400, Error: err.Error()}
	}
	if err != nil {
		return Reply{Status: 503, Error: "Cannot save service configuration"}
	}
	return Reply{Revision: rev, Status: 200}
}

var errUsedAdminNonce = errors.New("request already processed")

func (a *Authority) recordNonce(m Message) Reply {
	if err := a.consumeNonce(m); errors.Is(err, errUsedAdminNonce) {
		return Reply{Status: 409, Error: err.Error()}
	} else if err != nil {
		return Reply{Status: 503, Error: "Cannot persist administrator replay window"}
	}
	return Reply{}
}

// The supervisor keeps its own short replay window, including across restarts.
// A compromised web child cannot reset it by replacing the web database.
func (a *Authority) consumeNonce(m Message) error {
	path := a.Path + config.NoncesSuffix
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
	return fsutil.WriteAtomicFile(path, data, 0600)
}
