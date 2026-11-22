package builder

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"golang.org/x/sys/unix"
)

// Return nil when the source is unavailable. A readable, empty file produces
// an empty JSON array. Never expose comments or the contents of invalid lines.
func ConfiguredOrigins(path string) []string {
	file, err := readPackagesFile(path)
	if err != nil {
		return nil
	}
	origins, err := buildOrigins(file, nil)
	if err != nil {
		return nil
	}
	return origins
}

type ResolvedPort struct {
	Origin, Version string `json:"-"`
}

// Poudriere records the exact package name and version its ports tree resolved
// for this jail, including flavors, epochs and revisions, before starting jobs.
func ResolvedPorts(cfg config.Builder) map[string]ResolvedPort {
	root, err := os.OpenRoot(cfg.BaseFS)
	if err != nil {
		return nil
	}
	defer root.Close()
	log, err := root.OpenRoot(filepath.Join("data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports, "latest"))
	if err != nil {
		return nil
	}
	defer log.Close()
	const maxBytes = 32 << 20
	data := progressValue(log, "all_pkgs%", maxBytes)
	pkgColumn, originColumn := 0, 1
	if data == "" {
		// Older or incomplete runs may only have the post-trim queue.
		data = progressValue(log, "ports.queued", maxBytes)
		pkgColumn, originColumn = 1, 0
	}
	versions := make(map[string]ResolvedPort)
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pkg, origin := fields[pkgColumn], fields[originColumn]
		if len(pkg) > 512 || !packageinfo.JobPackage.MatchString(pkg) || !packageinfo.BuildOrigin.MatchString(origin) {
			continue
		}
		if name, version := packageinfo.SplitNameVersion(pkg); name != "" {
			versions[name] = ResolvedPort{origin, version}
		}
	}
	return versions
}

type BuildIssue struct {
	Package   string `json:"package"`
	Origin    string `json:"origin"`
	Phase     string `json:"phase,omitempty"`
	Reason    string `json:"reason,omitempty"`
	BlockedBy string `json:"blocked_by,omitempty"`
}

type BuildIssuesView struct {
	Started time.Time    `json:"started,omitzero"`
	Runs    []time.Time  `json:"runs"`
	Failed  []BuildIssue `json:"failed"`
	Skipped []BuildIssue `json:"skipped"`
}

type builtPackage struct {
	Origin       string `json:"origin"`
	Package      string `json:"package"`
	BuildSeconds *int64 `json:"build_seconds,omitempty"`
}

type BuildResultsView struct {
	BuildIssuesView
	Succeeded []builtPackage `json:"succeeded"`
}

type BuildLogPage struct {
	Text  string `json:"text"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	Size  int64  `json:"size"`
}

// Open each component relative to a pinned directory descriptor. Poudriere's
// "latest" and "errors" symlinks are never needed to identify a retained run.
func OpenLogPath(dir *os.File, path string, directory bool) (*os.File, error) {
	if !filepath.IsLocal(path) || filepath.Clean(path) != path || path == "." {
		return nil, fs.ErrInvalid
	}
	parent := dir
	for {
		part, rest, more := strings.Cut(path, "/")
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if more || directory {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(int(parent.Fd()), part, flags, 0)
		runtime.KeepAlive(parent)
		if parent != dir {
			parent.Close()
		}
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fd), part)
		if !more {
			return file, nil
		}
		parent, path = file, rest
	}
}

func OpenBulkLogs(cfg config.Builder) (*os.File, error) {
	if !filepath.IsAbs(cfg.BaseFS) || !config.BuildName.MatchString(cfg.Jail) || !config.BuildName.MatchString(cfg.Ports) {
		return nil, fs.ErrInvalid
	}
	// The host's configured BaseFS may itself be a root-managed symlink.
	// Pin its target, then forbid symlinks at every step below that anchor.
	base, err := os.OpenFile(cfg.BaseFS, os.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	return OpenLogPath(base, "data/logs/bulk", true)
}

func openBuildMaster(cfg config.Builder) (*os.File, error) {
	bulk, err := OpenBulkLogs(cfg)
	if err != nil {
		return nil, err
	}
	defer bulk.Close()
	return OpenLogPath(bulk, cfg.Jail+"-"+cfg.Ports, true)
}

func openBuildMetadata(run *os.File, name string) (*os.File, os.FileInfo, error) {
	f, err := OpenLogPath(run, ".poudriere."+name, false)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1) {
		err = errors.New("unexpected Poudriere result file")
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// The first two fields of each native result list are the origin and package.
func ScanBuildResults(ctx context.Context, run *os.File, name string, visit func(string, []string) bool) error {
	f, info, err := openBuildMetadata(run, "ports."+name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if info.Size() > 64<<20 {
		return errors.New("Poudriere result list exceeds 64 MiB")
	}
	// A live bulk run can append while we read. Only expose complete lines
	// from the snapshot; the next refresh picks up any newly finished line.
	reader := bufio.NewReader(io.NewSectionReader(f, 0, info.Size()))
	for lines := 0; ; lines++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadSlice('\n')
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if lines >= 250_000 {
			return errors.New("Poudriere result list exceeds 250,000 records")
		}
		entry := strings.TrimSpace(string(line))
		fields := strings.Fields(entry)
		if len(fields) < 2 || len(fields[0]) > 255 || !packageinfo.BuildOrigin.MatchString(fields[0]) || !packageinfo.ValidIssuePackage(fields[1]) {
			continue
		}
		if !visit(entry, fields) {
			return nil
		}
	}
}

func readIssueList(ctx context.Context, run *os.File, name string) ([]BuildIssue, error) {
	items := []BuildIssue{}
	seen := make(map[BuildIssue]struct{})
	err := ScanBuildResults(ctx, run, name, func(entry string, fields []string) bool {
		issue := BuildIssue{Origin: fields[0], Package: fields[1]}
		if name == "skipped" {
			if len(fields) != 3 || !packageinfo.ValidIssuePackage(fields[2]) {
				return true
			}
			issue.BlockedBy = fields[2]
		} else {
			if len(fields) >= 3 {
				issue.Phase = BuildText(fields[2])
			}
			if len(fields) >= 4 {
				// Some setups emit an empty reason followed by elapsed seconds.
				_, durationErr := strconv.ParseUint(fields[3], 10, 63)
				emptyReason := len(fields) == 4 && durationErr == nil && strings.HasSuffix(entry, "  "+fields[3])
				if !emptyReason && fields[3] != "???" {
					issue.Reason = BuildText(fields[3])
				}
			}
		}
		if _, exists := seen[issue]; !exists {
			seen[issue] = struct{}{}
			items = append(items, issue)
		}
		return true
	})
	return items, err
}

func ReadRunIssues(ctx context.Context, cfg config.Builder, started time.Time) ([]BuildIssue, []BuildIssue, error) {
	if started.IsZero() {
		return nil, nil, fs.ErrInvalid
	}
	bulk, err := OpenBulkLogs(cfg)
	if err != nil {
		return nil, nil, err
	}
	defer bulk.Close()
	run, err := OpenLogPath(bulk, cfg.Jail+"-"+cfg.Ports+"/"+started.UTC().Format(BuildNameFormat), true)
	if err != nil {
		return nil, nil, err
	}
	defer run.Close()
	return runIssues(ctx, run)
}

func runIssues(ctx context.Context, run *os.File) ([]BuildIssue, []BuildIssue, error) {
	failed, err := readIssueList(ctx, run, "failed")
	if err != nil {
		return nil, nil, err
	}
	skipped, err := readIssueList(ctx, run, "skipped")
	return failed, skipped, err
}

func hasBuildIssues(run *os.File) (bool, error) {
	for _, name := range []string{"failed", "skipped"} {
		f, info, err := openBuildMetadata(run, "ports."+name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		f.Close()
		if info.Size() > 0 {
			return true, nil
		}
	}
	return false, nil
}

func retainedBuildRuns(ctx context.Context, master *os.File) ([]time.Time, error) {
	entries, err := master.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	runs := []time.Time{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() {
			continue
		}
		t, err := time.Parse(BuildNameFormat, entry.Name())
		if err != nil || t.Format(BuildNameFormat) != entry.Name() {
			continue
		}
		runs = append(runs, t)
	}
	slices.SortFunc(runs, func(a, b time.Time) int { return b.Compare(a) })
	return runs, nil
}

func ReadBuildResults(ctx context.Context, cfg config.Builder, started time.Time) (BuildResultsView, error) {
	view := BuildResultsView{
		BuildIssuesView: BuildIssuesView{Runs: []time.Time{}, Failed: []BuildIssue{}, Skipped: []BuildIssue{}},
		Succeeded:       []builtPackage{},
	}
	master, err := openBuildMaster(cfg)
	if errors.Is(err, os.ErrNotExist) && started.IsZero() {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	defer master.Close()
	view.Runs, err = retainedBuildRuns(ctx, master)
	if err != nil {
		return view, err
	}
	// Read only the selected run's lists. New, successful and no-work runs
	// all take precedence over older failures when following the latest run.
	for len(view.Runs) > 0 {
		chosen := started
		if chosen.IsZero() {
			chosen = view.Runs[0]
		}
		run, err := OpenLogPath(master, chosen.UTC().Format(BuildNameFormat), true)
		if started.IsZero() && errors.Is(err, os.ErrNotExist) {
			view.Runs = view.Runs[1:] // Pruned since readdir.
			continue
		}
		if err != nil {
			return view, err
		}
		defer run.Close()
		view.Started = chosen.UTC()
		view.Failed, view.Skipped, err = runIssues(ctx, run)
		if err != nil {
			return view, err
		}
		seen := make(map[string]bool)
		err = ScanBuildResults(ctx, run, "built", func(line string, fields []string) bool {
			pkg := fields[1]
			if !seen[pkg] {
				seen[pkg] = true
				view.Succeeded = append(view.Succeeded, builtPackage{Origin: fields[0], Package: pkg, BuildSeconds: packageBuildSeconds("built", line, fields)})
			}
			return true
		})
		return view, err
	}
	if !started.IsZero() {
		return view, os.ErrNotExist
	}
	return view, nil
}

// Older Management clients use this endpoint to find their latest failure.
func ReadBuildIssues(ctx context.Context, cfg config.Builder, started time.Time) (BuildIssuesView, error) {
	view := BuildIssuesView{Runs: []time.Time{}, Failed: []BuildIssue{}, Skipped: []BuildIssue{}}
	master, err := openBuildMaster(cfg)
	if errors.Is(err, os.ErrNotExist) && started.IsZero() {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	defer master.Close()
	runs, err := retainedBuildRuns(ctx, master)
	if err != nil {
		return view, err
	}
	var firstReadErr error
	for _, t := range runs {
		if err := ctx.Err(); err != nil {
			return view, err
		}
		run, err := OpenLogPath(master, t.Format(BuildNameFormat), true)
		if errors.Is(err, os.ErrNotExist) {
			continue // Poudriere can prune a run after readdir.
		}
		if err != nil {
			if t.Equal(started) {
				return view, err
			}
			if firstReadErr == nil {
				firstReadErr = err
			}
			continue
		}
		issues, err := hasBuildIssues(run)
		selected := t.Equal(started) || (started.IsZero() && view.Started.IsZero() && issues)
		if err == nil && selected {
			view.Failed, view.Skipped, err = runIssues(ctx, run)
			issues = len(view.Failed)+len(view.Skipped) != 0
			if issues || !started.IsZero() {
				view.Started = t
			}
		}
		run.Close()
		if err != nil {
			if t.Equal(started) || ctx.Err() != nil {
				return view, err
			}
			if firstReadErr == nil {
				firstReadErr = err
			}
			continue
		}
		if issues {
			view.Runs = append(view.Runs, t)
		}
	}
	if !started.IsZero() && view.Started.IsZero() {
		return view, os.ErrNotExist
	}
	if view.Started.IsZero() && firstReadErr != nil {
		return view, firstReadErr
	}
	return view, nil
}

// _logfile() creates up to two additional links for the latest build of this
// exact package. Account for them without following symlinks. An unrelated
// linked file must never become a readable package log.
func expectedLogLinks(bulk *os.File, master, pkg string, info os.FileInfo) bool {
	n := uint64(info.Sys().(*syscall.Stat_t).Nlink)
	if n == 1 {
		return true
	}
	if n == 0 || n > 3 {
		return false
	}
	name, version := packageinfo.SplitBuildPackage(pkg)
	if name == "" {
		return false
	}
	aliases := []string{master + "/latest-per-pkg/" + pkg + ".log", "latest-per-pkg/" + name + "/" + version + "/" + master + ".log"}
	for _, alias := range aliases {
		f, err := OpenLogPath(bulk, alias, false)
		if err != nil {
			continue
		}
		aliasInfo, err := f.Stat()
		f.Close()
		if err == nil && os.SameFile(info, aliasInfo) {
			n--
		}
	}
	return n == 1
}

// Status and builders are both published atomically by Poudriere's bset.
func diagnosticValue(run *os.File, name string, limit int64) (string, error) {
	f, info, err := openBuildMetadata(run, name)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info.Size() > limit {
		return "", errors.New("Poudriere job status exceeds the size limit")
	}
	data, err := io.ReadAll(io.NewSectionReader(f, 0, info.Size()))
	return strings.TrimSpace(string(data)), err
}

func packageHasBuildLog(ctx context.Context, run *os.File, pkg string) (bool, error) {
	// A skipped package has no build log. Only trust a completed result
	// or the active job of a builder explicitly listed in this run.
	var found bool
	visit := func(_ string, fields []string) bool {
		found = fields[1] == pkg
		return !found
	}
	if err := ScanBuildResults(ctx, run, "failed", visit); err != nil || found {
		return found, err
	}
	builders, err := diagnosticValue(run, "builders", 1<<20)
	if err != nil {
		return false, err
	}
	for id := range strings.FieldsSeq(builders) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, err := strconv.ParseUint(id, 10, 32); err != nil {
			continue
		}
		status, err := diagnosticValue(run, "status."+id, 1024)
		if err != nil {
			return false, err
		}
		fields := strings.Split(status, ":")
		if len(fields) >= 3 && fields[0] != "idle" && jobPhase.MatchString(fields[0]) && packageinfo.BuildOrigin.MatchString(fields[1]) && fields[2] == pkg {
			return true, nil
		}
	}
	err = ScanBuildResults(ctx, run, "built", visit)
	return found, err
}

const buildLogPageBytes = 32 << 10

func ReadBuildLog(ctx context.Context, cfg config.Builder, started time.Time, pkg string, before *int64) (BuildLogPage, error) {
	var page BuildLogPage
	if started.IsZero() || !packageinfo.ValidIssuePackage(pkg) || before != nil && *before < 0 {
		return page, fs.ErrInvalid
	}
	bulk, err := OpenBulkLogs(cfg)
	if err != nil {
		return page, err
	}
	defer bulk.Close()
	master := cfg.Jail + "-" + cfg.Ports
	run, err := OpenLogPath(bulk, master+"/"+started.UTC().Format(BuildNameFormat), true)
	if err != nil {
		return page, err
	}
	defer run.Close()
	ok, err := packageHasBuildLog(ctx, run, pkg)
	if err != nil {
		return page, err
	}
	if !ok {
		return page, os.ErrNotExist
	}
	file, err := OpenLogPath(run, "logs/"+pkg+".log", false)
	if err != nil {
		return page, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return page, err
	}
	if !info.Mode().IsRegular() || !expectedLogLinks(bulk, master, pkg, info) {
		return page, errors.New("unexpected Poudriere package log")
	}
	page.Size, page.End = info.Size(), info.Size()
	if before != nil {
		if *before > page.Size {
			return page, fs.ErrInvalid
		}
		page.End = *before
	}
	page.Start = max(0, page.End-buildLogPageBytes)
	data, err := io.ReadAll(io.NewSectionReader(file, page.Start, page.End-page.Start))
	if err != nil {
		return page, err
	}
	if int64(len(data)) != page.End-page.Start {
		return page, errors.New("Poudriere package log changed while reading")
	}
	// Offsets refer to bytes in the native file. Pages opened in sequence
	// keep complete UTF-8 characters even when the nominal boundary splits one.
	for i := 0; page.Start != 0 && i < utf8.UTFMax-1 && len(data) > 0 && !utf8.RuneStart(data[0]); i++ {
		page.Start++
		data = data[1:]
	}
	page.Text = terminalEscape.ReplaceAllString(strings.ToValidUTF8(string(data), "�"), "")
	page.Text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' {
			return -1
		}
		return r
	}, page.Text)
	return page, ctx.Err()
}
