package poudrine

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

	"golang.org/x/sys/unix"
)

type buildIssue struct {
	Package   string `json:"package"`
	Origin    string `json:"origin"`
	Phase     string `json:"phase,omitempty"`
	Reason    string `json:"reason,omitempty"`
	BlockedBy string `json:"blocked_by,omitempty"`
}

type buildIssuesView struct {
	Started time.Time    `json:"started,omitzero"`
	Runs    []time.Time  `json:"runs"`
	Failed  []buildIssue `json:"failed"`
	Skipped []buildIssue `json:"skipped"`
}

type buildLogPage struct {
	Text  string `json:"text"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	Size  int64  `json:"size"`
}

// Open each component relative to a pinned directory descriptor. Poudriere's
// "latest" and "errors" symlinks are never needed to identify a retained run.
func openLogPath(dir *os.File, path string, directory bool) (*os.File, error) {
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

func openBulkLogs(cfg BuilderConfig) (*os.File, error) {
	if !filepath.IsAbs(cfg.BaseFS) || !buildName.MatchString(cfg.Jail) || !buildName.MatchString(cfg.Ports) {
		return nil, fs.ErrInvalid
	}
	// The host's configured BaseFS may itself be a root-managed symlink.
	// Pin its target, then forbid symlinks at every step below that anchor.
	base, err := os.OpenFile(cfg.BaseFS, os.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	return openLogPath(base, "data/logs/bulk", true)
}

func openBuildMetadata(run *os.File, name string) (*os.File, os.FileInfo, error) {
	f, err := openLogPath(run, ".poudriere."+name, false)
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

func validIssuePackage(pkg string) bool {
	return len(pkg) <= 255 && jobPackage.MatchString(pkg)
}

// The first two fields of each native result list are the origin and package.
func scanBuildResults(ctx context.Context, run *os.File, name string, visit func(string, []string) bool) error {
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
		if len(fields) < 2 || len(fields[0]) > 255 || !buildOrigin.MatchString(fields[0]) || !validIssuePackage(fields[1]) {
			continue
		}
		if !visit(entry, fields) {
			return nil
		}
	}
}

func readIssueList(ctx context.Context, run *os.File, name string) ([]buildIssue, error) {
	items := []buildIssue{}
	seen := make(map[buildIssue]struct{})
	err := scanBuildResults(ctx, run, name, func(entry string, fields []string) bool {
		issue := buildIssue{Origin: fields[0], Package: fields[1]}
		if name == "skipped" {
			if len(fields) != 3 || !validIssuePackage(fields[2]) {
				return true
			}
			issue.BlockedBy = fields[2]
		} else {
			if len(fields) >= 3 {
				issue.Phase = buildText(fields[2])
			}
			if len(fields) >= 4 {
				// Some setups emit an empty reason followed by elapsed seconds.
				_, durationErr := strconv.ParseUint(fields[3], 10, 63)
				emptyReason := len(fields) == 4 && durationErr == nil && strings.HasSuffix(entry, "  "+fields[3])
				if !emptyReason && fields[3] != "???" {
					issue.Reason = buildText(fields[3])
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

func readRunIssues(ctx context.Context, cfg BuilderConfig, started time.Time) ([]buildIssue, []buildIssue, error) {
	if started.IsZero() {
		return nil, nil, fs.ErrInvalid
	}
	bulk, err := openBulkLogs(cfg)
	if err != nil {
		return nil, nil, err
	}
	defer bulk.Close()
	run, err := openLogPath(bulk, cfg.Jail+"-"+cfg.Ports+"/"+started.UTC().Format(buildNameFormat), true)
	if err != nil {
		return nil, nil, err
	}
	defer run.Close()
	return runIssues(ctx, run)
}

func runIssues(ctx context.Context, run *os.File) ([]buildIssue, []buildIssue, error) {
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

func readBuildIssues(ctx context.Context, cfg BuilderConfig, started time.Time) (buildIssuesView, error) {
	view := buildIssuesView{Runs: []time.Time{}, Failed: []buildIssue{}, Skipped: []buildIssue{}}
	bulk, err := openBulkLogs(cfg)
	if errors.Is(err, os.ErrNotExist) && started.IsZero() {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	defer bulk.Close()
	master, err := openLogPath(bulk, cfg.Jail+"-"+cfg.Ports, true)
	if errors.Is(err, os.ErrNotExist) && started.IsZero() {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	defer master.Close()

	entries, err := master.ReadDir(-1)
	if err != nil {
		return view, err
	}
	// Canonical Poudrine run names sort chronologically. Select the newest
	// actual issue, even if newer clean or still-preparing runs exist.
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(b.Name(), a.Name()) })
	var firstReadErr error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return view, err
		}
		if !entry.IsDir() {
			continue
		}
		t, err := time.Parse(buildNameFormat, entry.Name())
		if err != nil || t.Format(buildNameFormat) != entry.Name() {
			continue
		}
		run, err := openLogPath(master, entry.Name(), true)
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
	dash := strings.LastIndexByte(pkg, '-')
	if dash <= 0 || dash == len(pkg)-1 {
		return false
	}
	aliases := []string{master + "/latest-per-pkg/" + pkg + ".log", "latest-per-pkg/" + pkg[:dash] + "/" + pkg[dash+1:] + "/" + master + ".log"}
	for _, alias := range aliases {
		f, err := openLogPath(bulk, alias, false)
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
	if err := scanBuildResults(ctx, run, "failed", visit); err != nil || found {
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
		if len(fields) >= 3 && fields[0] != "idle" && jobPhase.MatchString(fields[0]) && buildOrigin.MatchString(fields[1]) && fields[2] == pkg {
			return true, nil
		}
	}
	err = scanBuildResults(ctx, run, "built", visit)
	return found, err
}

const buildLogPageBytes = 32 << 10

func readBuildLog(ctx context.Context, cfg BuilderConfig, started time.Time, pkg string, before *int64) (buildLogPage, error) {
	var page buildLogPage
	if started.IsZero() || !validIssuePackage(pkg) || before != nil && *before < 0 {
		return page, fs.ErrInvalid
	}
	bulk, err := openBulkLogs(cfg)
	if err != nil {
		return page, err
	}
	defer bulk.Close()
	master := cfg.Jail + "-" + cfg.Ports
	run, err := openLogPath(bulk, master+"/"+started.UTC().Format(buildNameFormat), true)
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
	file, err := openLogPath(run, "logs/"+pkg+".log", false)
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
