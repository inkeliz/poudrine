package poudrine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// The worker runs separately from the HTTP servers. Its paths and configuration
// must be controlled by the administrator. Only origins come from the web DB.
type BuilderConfig struct {
	Release, Jail, Ports                 string
	ConfigDir, BaseFS, Distfiles         string
	SigningKey, PackagesFile, StatusFile string
	Jobs                                 int
	MakeJobsNumber, MakeJobsNumberLimit  int
	Interval                             time.Duration
	WorkingHours                         workingHours
}

func DefaultBuilderConfig() BuilderConfig {
	return BuilderConfig{
		Jail: "latest", Ports: "main", ConfigDir: "/usr/local/etc/poudrine",
		BaseFS: "/usr/local/poudriere", Distfiles: "/var/cache/distfiles",
		SigningKey: "/usr/local/etc/poudriere.key", PackagesFile: "/usr/local/etc/poudriere.d/packages.txt",
		StatusFile: "/var/run/poudrine/build.json", Jobs: 6, Interval: 15 * time.Minute,
	}
}

func (c BuilderConfig) RepositoryDir() string {
	return filepath.Join(c.BaseFS, "data", "packages", c.Jail+"-"+c.Ports)
}

type BuildStatus struct {
	PoudriereBytes   *int64         `json:"poudriere_bytes,omitempty"`
	Active           bool           `json:"active"`
	ManualPending    bool           `json:"manual_pending"`
	RetryPending     int            `json:"retry_pending,omitempty"`
	Paused           bool           `json:"paused,omitempty"`
	WindowOpens      time.Time      `json:"window_opens,omitzero"`
	PauseError       string         `json:"pause_error,omitempty"`
	Trigger          string         `json:"trigger,omitempty"`
	NextRun          time.Time      `json:"next_run,omitzero"`
	NextDelaySeconds float64        `json:"next_delay_seconds,omitempty"`
	NextReason       string         `json:"next_reason,omitempty"`
	Origins          *int           `json:"origins,omitempty"`
	Outcome          string         `json:"outcome,omitempty"`
	Progress         *BuildProgress `json:"progress,omitempty"`
	Jobs             []BuildJob     `json:"jobs,omitempty"`
	Stage            string         `json:"stage"`
	Message          string         `json:"message"`
	Log              []string       `json:"log"`
	Started          time.Time      `json:"started"`
	Finished         time.Time      `json:"finished"`
}

func ReadBuildStatus(path string) (BuildStatus, error) {
	var wire struct {
		BuildStatus
		Active *bool `json:"active"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return BuildStatus{}, err
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return BuildStatus{}, err
	}
	if wire.Active != nil {
		wire.BuildStatus.Active = *wire.Active
	} else {
		// Older standalone workers publish phases without the active flag.
		switch wire.Stage {
		case "jail", "ports", "preparing", "building":
			wire.BuildStatus.Active = true
		}
	}
	return wire.BuildStatus, nil
}

type Builder struct {
	cfg             BuilderConfig
	executable      string
	tracked         func(context.Context) ([]string, error)
	finished        func(context.Context, error)
	wake            chan struct{}
	hoursChanged    chan struct{}
	hours           workingHours
	mu              sync.Mutex
	status          BuildStatus
	retryOrigins    map[string]struct{}
	lastWrite       time.Time
	ready           bool
	onBuildFinished func(context.Context, BuildStatus)
	monitorHistory  func(context.Context)
	restartHold     *buildRestartHold
	commandRunning  bool
	reexecPending   bool
	savedSchedule   buildSchedule
}

var buildName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]*$`)
var buildPath = regexp.MustCompile(`^/[A-Za-z0-9/_.+-]+$`)
var buildRelease = regexp.MustCompile(`^([1-9][0-9]*)\.(0|[1-9][0-9]*)-RELEASE(-p(0|[1-9][0-9]*))?$`)
var buildOrigin = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]*/[A-Za-z0-9][A-Za-z0-9_.+-]*(@[A-Za-z0-9][A-Za-z0-9_.+-]*)?(~[A-Za-z0-9][A-Za-z0-9_.+-]*)?$`)

func (c BuilderConfig) validate() error {
	for _, field := range []struct{ name, value string }{{"jail", c.Jail}, {"ports", c.Ports}} {
		if !buildName.MatchString(field.value) {
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
		if _, _, err := jailRelease(c.Release); err != nil {
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

func NewBuilder(c BuilderConfig, tracked func(context.Context) ([]string, error), finished func(context.Context, error)) (*Builder, error) {
	if tracked == nil {
		return nil, errors.New("missing tracked ports reader")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &Builder{cfg: c, executable: exe, tracked: tracked, finished: finished, wake: make(chan struct{}, 1),
		hours: c.WorkingHours, hoursChanged: make(chan struct{}, 1),
		status: BuildStatus{Stage: "waiting", Message: "Waiting for the build worker"}}, nil
}

func (b *Builder) Wake() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// RequestBuild wakes an idle worker or queues a single follow-up to an active
// build. Concurrent requests for that pending run share the same queue slot.
func (b *Builder) RequestBuild() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.status.ManualPending {
		return false
	}
	b.status.ManualPending = true
	if b.ready {
		b.persistLocked()
	}
	b.Wake()
	return true
}

func (b *Builder) snapshotLocked() BuildStatus {
	s := b.status
	s.RetryPending = len(b.retryOrigins)
	s.Log = slices.Clone(s.Log)
	s.Jobs = slices.Clone(s.Jobs)
	for i := range s.Jobs {
		job := &s.Jobs[i]
		job.ProgressPercent = cloneValue(job.ProgressPercent)
		job.ElapsedSeconds = cloneValue(job.ElapsedSeconds)
		job.CPUPercent = cloneValue(job.CPUPercent)
		if s.Paused && job.CPUPercent != nil {
			// ps's decaying CPU average can outlive a confirmed SIGSTOP.
			*job.CPUPercent = 0
		}
		job.MemoryBytes = cloneValue(job.MemoryBytes)
	}
	s.Origins = cloneValue(s.Origins)
	s.Progress = cloneValue(s.Progress)
	s.PoudriereBytes = cloneValue(s.PoudriereBytes)
	if !s.WindowOpens.IsZero() {
		s.NextReason = "working_hours"
		s.NextRun = s.WindowOpens
		s.NextDelaySeconds = 0
	} else if s.RetryPending > 0 || s.ManualPending {
		s.NextReason = "manual"
		if s.RetryPending > 0 {
			s.NextReason = "retry"
		}
		s.NextRun = time.Time{}
		s.NextDelaySeconds = 0
	} else if s.NextDelaySeconds > 0 {
		// A manual update keeps the existing maintenance deadline unless
		// the run crosses it, in which case the interval starts at finish.
		if s.NextRun.After(time.Now()) {
			s.NextDelaySeconds = 0
		} else {
			s.NextRun = time.Time{}
		}
	}
	return s
}

func cloneValue[T any](p *T) *T {
	if p == nil {
		return nil
	}
	return new(*p)
}

func (b *Builder) Status() BuildStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotLocked()
}

// The HTTP process reads the build status and published packages under a
// separate uid. Give newly created public directories traversal permission
// even if the person who starts the worker uses umask 077. Existing directory
// permissions stay under the administrator's control.
func makeBuilderSharedDir(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent != path {
		if err := makeBuilderSharedDir(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(path, 0755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return os.Chmod(path, 0755)
}

func (b *Builder) persistLocked() {
	data, err := json.Marshal(b.snapshotLocked())
	if err == nil {
		err = writeAtomicFile(b.cfg.StatusFile, data, 0644)
	}
	if err != nil {
		log.Printf("recording Poudriere status: %v", err)
	}
	b.lastWrite = time.Now()
}

func (b *Builder) stage(s, msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status.Stage = s
	b.status.Message = buildText(msg)
	b.persistLocked()
}

// os/exec serializes calls to a shared stdout/stderr writer.
type buildOutput struct {
	b       *Builder
	partial []byte
}

var terminalEscape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\|$))`)

// Keep control sequences and binary data out of web-visible status. With
// 40 lines capped at 1 KiB, even JSON escaping fits in the supervisor frame.
func buildText(s string) string {
	s = strings.ToValidUTF8(s[:min(len(s), 1024)], "?")
	s = terminalEscape.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

func (w *buildOutput) line() {
	line := buildText(string(w.partial))
	w.partial = w.partial[:0]
	if line == "" {
		return
	}
	b := w.b
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status.Log = append(b.status.Log, line)
	if len(b.status.Log) > 40 {
		b.status.Log = b.status.Log[1:]
	}
	if time.Since(b.lastWrite) > time.Second {
		b.persistLocked()
	}
}

func (w *buildOutput) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		line, tail, complete := bytes.Cut(p, []byte{'\n'})
		w.partial = append(w.partial, line[:min(len(line), 1024-len(w.partial))]...)
		if complete {
			w.line()
		}
		p = tail
	}
	return n, nil
}

func (b *Builder) command(ctx context.Context, names bool, args ...string) (string, error) {
	if err := b.waitForWindow(ctx, false); err != nil {
		return "", err
	}
	out := &buildOutput{b: b}
	defer out.line()
	var list bytes.Buffer
	var stdout io.Writer = out
	if names {
		stdout = &list
	}
	if err := b.runWorkCommand(ctx, args, stdout, out); err != nil {
		return "", fmt.Errorf("poudriere %s: %w", strings.Join(args, " "), err)
	}
	return list.String(), nil
}

func (b *Builder) configure() error {
	c := b.cfg
	if err := makeBuilderSharedDir(c.BaseFS); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Join(c.ConfigDir, "poudriere.d"), c.Distfiles, filepath.Dir(c.SigningKey), filepath.Dir(c.PackagesFile)} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	if _, err := os.Stat(c.SigningKey); errors.Is(err, fs.ErrNotExist) {
		key, err := rsa.GenerateKey(rand.Reader, 3072)
		if err != nil {
			return err
		}
		data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		if err := writeAtomicFile(c.SigningKey, data, 0400); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	config := fmt.Sprintf("NO_ZFS=yes\nBASEFS=%s\nFREEBSD_HOST=https://download.FreeBSD.org\nRESOLV_CONF=/etc/resolv.conf\nDISTFILES_CACHE=%s\nPARALLEL_JOBS=%d\nUSE_TMPFS=no\nPKG_REPO_SIGNING_KEY=%s\nATOMIC_PACKAGE_REPOSITORY=yes\n", c.BaseFS, c.Distfiles, c.Jobs, c.SigningKey)
	if (c.MakeJobsNumber > 1 || c.MakeJobsNumberLimit > 1) && c.MakeJobsNumber != 1 && c.MakeJobsNumberLimit != 1 {
		config += "ALLOW_MAKE_JOBS=yes\n"
	}
	config += poudrierePauseClockConfig
	var makeConf strings.Builder
	if c.MakeJobsNumber > 0 {
		fmt.Fprintf(&makeConf, "MAKE_JOBS_NUMBER=%d\n", c.MakeJobsNumber)
	}
	if c.MakeJobsNumberLimit > 0 {
		fmt.Fprintf(&makeConf, "MAKE_JOBS_NUMBER_LIMIT=%d\n", c.MakeJobsNumberLimit)
	}
	if err := writeWorkerConfig(c.makeJobsFile(), makeConf.String()); err != nil {
		return err
	}
	return writeWorkerConfig(filepath.Join(c.ConfigDir, "poudriere.conf"), config)
}

// Poudriere appends each plugin's make.conf to the build jail's make.conf.
// A dedicated fragment leaves administrator-owned port settings intact.
func (c BuilderConfig) makeJobsFile() string {
	return filepath.Join(c.ConfigDir, "poudriere.d", "hooks", "plugins", "poudrine-jobs", "make.conf")
}

func writeWorkerConfig(path, content string) error {
	const marker = "# Generated by Poudrine worker\n"
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if content == "" {
		if bytes.HasPrefix(old, []byte(marker)) {
			return os.Remove(path)
		}
		return nil
	}
	if err == nil && !bytes.HasPrefix(old, []byte(marker)) {
		return fmt.Errorf("existing %s is administrator-managed, use a separate worker config directory", path)
	}
	content = marker + content
	if string(old) == content {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return writeAtomicFile(path, []byte(content), 0644)
}

func readPackagesFile(path string) (string, error) {
	// Root-managed symlinks are supported. Check the opened descriptor so
	// a pipe or device cannot block an inventory poll or worker sync.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("packages_file must be a regular file")
	}
	data, err := io.ReadAll(f)
	return string(data), err
}

func buildOrigins(file string, requested []string) ([]string, error) {
	// Keep explicitly configured origins as well as client-requested ports.
	origins := append(make([]string, 0, len(requested)), requested...)
	for line := range strings.SplitSeq(file, "\n") {
		line, _, _ = strings.Cut(line, "#")
		if line = strings.TrimSpace(line); line != "" {
			origins = append(origins, line)
		}
	}
	for _, origin := range origins {
		if len(origin) > 255 || !buildOrigin.MatchString(origin) {
			return nil, fmt.Errorf("invalid tracked port origin %q", origin)
		}
	}
	slices.Sort(origins)
	return slices.Compact(origins), nil
}

func (b *Builder) syncOrigins(dbOrigins []string) (string, error) {
	old, readErr := readPackagesFile(b.cfg.PackagesFile)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return "", readErr
	}
	origins, err := buildOrigins(old, dbOrigins)
	if err != nil {
		return "", err
	}
	s := strings.Join(origins, "\n")
	if s != "" {
		s += "\n"
	}
	if s != old || errors.Is(readErr, fs.ErrNotExist) {
		err = writeAtomicFile(b.cfg.PackagesFile, []byte(s), 0644)
	}
	return s, err
}

func (b *Builder) ensureTree(ctx context.Context, kind, name string, createArgs []string) error {
	names, err := b.command(ctx, true, kind, "-l", "-q", "-n")
	if err != nil {
		return err
	}
	opt := "-p"
	if kind == "jail" {
		opt = "-j"
	}
	if slices.Contains(strings.Fields(names), name) {
		_, err = b.command(ctx, false, kind, "-u", opt, name)
		return err
	}
	_, err = b.command(ctx, false, append([]string{kind, "-c", opt, name}, createArgs...)...)
	return err
}

func jailRelease(value string) (int, string, error) {
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

func (b *Builder) maintain(ctx context.Context) error {
	release := b.cfg.Release
	if release == "" {
		var err error
		release, err = hostUserlandRelease(ctx)
		if err != nil {
			return fmt.Errorf("detecting FreeBSD userland: %w", err)
		}
	}
	major, minor, err := jailRelease(release)
	if err != nil {
		return err
	}
	majorText := strconv.Itoa(major)
	args := []string{"-v", majorText + "." + minor + "-RELEASE", "-m", "http"}
	if major >= 15 {
		// pkgbase jail install in poudriere 3.4 takes the ABI major as -v.
		args = []string{"-v", majorText, "-m", "pkgbase=base_release_" + minor, "-U", "https://pkg.FreeBSD.org"}
	}
	b.stage("jail", "Preparing jail "+b.cfg.Jail)
	if err := b.ensureTree(ctx, "jail", b.cfg.Jail, args); err != nil {
		return err
	}
	b.stage("ports", "Updating ports tree "+b.cfg.Ports)
	return b.ensureTree(ctx, "ports", b.cfg.Ports, []string{"-m", "git+https", "-B", "main"})
}

func (b *Builder) Run(ctx context.Context) error {
	if err := os.MkdirAll(b.cfg.ConfigDir, 0755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(b.cfg.ConfigDir, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("locking worker directory: %w", err)
	}
	if err = makeBuilderSharedDir(filepath.Dir(b.cfg.StatusFile)); err != nil {
		return err
	}
	if err = b.configure(); err != nil {
		return err
	}
	b.mu.Lock()
	b.ready = true
	if b.status.Stage == "waiting" {
		b.status.Message = "Checking tracked ports"
	}
	schedule := b.savedSchedule
	b.persistLocked()
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.ready = false
		b.mu.Unlock()
	}()
	var monitors sync.WaitGroup
	monitorCtx, stopMonitors := context.WithCancel(ctx)
	monitors.Go(func() { b.monitorStorage(monitorCtx) })
	if b.monitorHistory != nil {
		monitors.Go(func() { b.monitorHistory(monitorCtx) })
	}
	defer monitors.Wait()
	defer stopMonitors()
	defer func() {
		b.mu.Lock()
		b.savedSchedule = schedule
		b.mu.Unlock()
	}()

	for ctx.Err() == nil {
		if err := b.waitForWindow(ctx, true); err != nil {
			return err
		}
		b.checkBuild(ctx, &schedule)
		// Check newly tracked origins at least every thirty seconds, and
		// wake precisely at the configured build or retry deadline.
		deadline := schedule.maintenance
		if !schedule.retry.IsZero() {
			deadline = schedule.retry
		}
		if _, boundary := b.currentWorkingHours().state(time.Now()); !boundary.IsZero() && boundary.Before(deadline) {
			deadline = boundary
		}
		timer := time.NewTimer(min(30*time.Second, time.Until(deadline)))
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-b.wake:
		}
		timer.Stop()
	}
	return ctx.Err()
}

type buildSchedule struct {
	maintenance      time.Time
	retry            time.Time
	retryMaintenance bool
	lastOrigins      string
}

func (b *Builder) checkBuild(ctx context.Context, schedule *buildSchedule) {
	now := time.Now().UTC()
	b.mu.Lock()
	if b.restartHold != nil || b.reexecPending {
		b.mu.Unlock()
		return
	}
	retryOrigins := b.retryOrigins
	if len(retryOrigins) > 0 {
		next := schedule.maintenance
		if !schedule.retry.IsZero() {
			next = schedule.retry
		}
		b.retryOrigins = nil
		b.beginLocked(now, "retry", new(len(retryOrigins)), next, false)
	}
	manual := b.status.ManualPending
	b.mu.Unlock()

	if len(retryOrigins) > 0 {
		b.retryPackages(ctx, now, schedule, retryOrigins)
		return
	}
	if !manual && now.Before(schedule.retry) {
		return
	}
	origins, err := b.tracked(ctx)
	var list string
	var originCount *int
	if err == nil {
		list, err = b.syncOrigins(origins)
		if err == nil {
			originCount = new(strings.Count(list, "\n"))
		}
	}
	if ctx.Err() != nil {
		return
	}
	due := !now.Before(schedule.maintenance)
	trigger := "tracked"
	switch {
	case manual:
		trigger = "manual"
	case !schedule.retry.IsZero():
		trigger = "retry"
	case due:
		trigger = "scheduled"
	case err == nil && list == schedule.lastOrigins:
		return
	}
	maintain := due || manual || schedule.retryMaintenance
	if !b.begin(now, trigger, originCount, schedule.maintenance, maintain) {
		return
	}
	if err == nil && maintain {
		err = b.maintain(ctx)
	}
	schedule.retryMaintenance = maintain && err != nil
	bulk := err == nil && list != "" && ctx.Err() == nil
	if bulk {
		err = b.bulk(ctx, now, b.cfg.PackagesFile, true)
	}
	finish := time.Now().UTC()
	next, nextReason := time.Time{}, ""
	switch {
	case ctx.Err() != nil:
		// The worker is stopping.
	case err != nil:
		schedule.retry = finish.Add(min(b.cfg.Interval, 5*time.Minute))
		next, nextReason = schedule.retry, "retry"
	default:
		schedule.retry = time.Time{}
		schedule.lastOrigins = list
		if maintain && !finish.Before(schedule.maintenance) {
			schedule.maintenance = finish.Add(b.cfg.Interval)
		}
		next, nextReason = schedule.maintenance, "scheduled"
	}
	b.complete(ctx, err, bulk, finish, next, nextReason)
}

func (b *Builder) begin(now time.Time, trigger string, origins *int, scheduled time.Time, maintain bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.restartHold != nil || b.reexecPending {
		return false
	}
	b.beginLocked(now, trigger, origins, scheduled, maintain)
	return true
}

func (b *Builder) beginLocked(now time.Time, trigger string, origins *int, scheduled time.Time, maintain bool) {
	b.status = BuildStatus{
		PoudriereBytes: b.status.PoudriereBytes,
		Active:         true, Stage: "preparing", Message: "Preparing build", Started: now,
		Trigger: trigger, Origins: origins, ManualPending: b.status.ManualPending && trigger != "manual",
		NextRun: scheduled, NextReason: "scheduled",
	}
	if maintain {
		b.status.NextDelaySeconds = b.cfg.Interval.Seconds()
	}
	b.persistLocked()
}

func (b *Builder) complete(ctx context.Context, err error, bulk bool, finished, next time.Time, nextReason string) {
	b.mu.Lock()
	b.status.Active = false
	b.status.Jobs = nil
	b.status.Finished = finished
	b.status.NextRun = next
	b.status.NextDelaySeconds = 0
	b.status.NextReason = nextReason
	b.status.Stage = "idle"
	b.status.Message = "Build completed"
	b.status.Outcome = "succeeded"
	switch {
	case ctx.Err() != nil:
		b.status.Outcome = "interrupted"
		b.status.Stage = "interrupted"
		b.status.Message = "Worker stopped during this run"
	case err != nil:
		b.status.Stage = "error"
		b.status.Outcome = "failed"
		b.status.Message = buildText(err.Error())
		log.Printf("Poudriere worker: %v", err)
	case !bulk:
		b.status.Message = "Maintenance complete. No ports are tracked yet."
	}
	b.persistLocked()
	status := b.snapshotLocked()
	b.mu.Unlock()
	if bulk && b.onBuildFinished != nil {
		b.onBuildFinished(ctx, status)
	}
	if b.finished != nil {
		b.finished(ctx, err)
	}
}
