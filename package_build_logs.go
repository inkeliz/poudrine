package poudrine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Poudriere appends an origin and package name to these result lists. The
// elapsed column is per-job runtime, not an offset from the bulk start.
type packageLogCursor struct {
	file   os.FileInfo
	offset int64
}

func (c *packageLogCursor) read(ctx context.Context, dir *os.Root, started time.Time, file, outcome string, active bool, save func(context.Context, []BuildRecord) error) error {
	f, err := dir.OpenFile(".poudriere.ports."+file, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("invalid Poudriere package result file")
	}
	offset := c.offset
	if c.file == nil || !os.SameFile(c.file, info) || info.Size() < offset || info.Size() == offset && !info.ModTime().Equal(c.file.ModTime()) {
		offset = 0
	}
	if info.Size() == offset {
		return nil
	}
	scanner := bufio.NewScanner(io.NewSectionReader(f, offset, info.Size()-offset))
	// Process only complete Poudriere lines. A killed writer may leave a
	// partial final line, even when preceding entries were fully recorded.
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if n := bytes.IndexByte(data, '\n'); n >= 0 {
			return n + 1, data[:n], nil
		}
		if atEOF {
			return len(data), nil, nil
		}
		return 0, nil, nil
	})
	var batch []BuildRecord
	lineOffset, nextOffset := offset, info.Size()
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := lineOffset
		lineOffset += int64(len(scanner.Bytes()) + 1) // Poudriere writes LF-terminated lines.
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		if len(fields) < 2 || !buildOrigin.MatchString(fields[0]) || len(fields[1]) > 255 || !jobPackage.MatchString(fields[1]) {
			continue
		}
		finished, abi, err := packageFinish(dir, fields[0], fields[1], started, active)
		if err != nil {
			return err
		}
		if finished.IsZero() {
			if active {
				nextOffset = min(nextOffset, start)
			}
			continue
		}
		record := BuildRecord{Started: started, Package: fields[1], Finished: finished, Outcome: outcome, Origin: fields[0], ABI: abi}
		// Success lists contain origin, package and seconds. Failure lists
		// additionally contain phase and reason. Older and crashed builders
		// can omit the duration entirely. An empty failure reason leaves two
		// literal spaces between phase and elapsed in Poudriere's output.
		emptyReason := file == "failed" && len(fields) == 4 && strings.HasSuffix(line, "  "+fields[3])
		if file == "built" && len(fields) == 3 || file == "failed" && len(fields) == 5 || emptyReason {
			if seconds, err := strconv.ParseUint(fields[len(fields)-1], 10, 63); err == nil {
				record.BuildSeconds = new(int64(seconds))
			}
		}
		batch = append(batch, record)
		if len(batch) == historyBatchSize {
			if err := save(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(batch) > 0 {
		if err := save(ctx, batch); err != nil {
			return err
		}
	}
	c.offset, c.file = min(nextOffset, lineOffset), info
	return nil
}

// Poudriere appends the result before cleaning the work directory and writing
// the final dated log footer. Wait for that footer during an active run.
// For retained logs from older versions, their last write is the best timestamp.
func packageFinish(dir *os.Root, origin, pkg string, started time.Time, active bool) (time.Time, string, error) {
	f, err := dir.OpenFile(filepath.Join("logs", pkg+".log"), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return time.Time{}, "", err
	}
	if !st.Mode().IsRegular() {
		return time.Time{}, "", nil
	}
	offset := max(int64(0), st.Size()-4096)
	tail, err := io.ReadAll(io.NewSectionReader(f, offset, st.Size()-offset))
	if err != nil {
		return time.Time{}, "", err
	}
	text := string(tail)
	marker := "build of " + origin + " | " + pkg + " ended at "
	at := strings.LastIndex(text, marker)
	if at >= 0 {
		line, _, complete := strings.Cut(text[at+len(marker):], "\n")
		stamp, err := time.ParseInLocation(time.UnixDate, strings.TrimSpace(line), time.Local)
		if err == nil && complete {
			zone, _ := stamp.Zone()
			localZone, _ := stamp.In(time.Local).Zone()
			if (zone == "UTC" || zone == "GMT" || zone == localZone) && !stamp.Before(started.Truncate(time.Second)) && !stamp.After(time.Now()) {
				// Preserve the footer date when restoring logs, and use its file's
				// fractional second when both clocks agree. A browser may have
				// subscribed between two builds that finished in the same second.
				if mtime := st.ModTime(); mtime.Truncate(time.Second).Equal(stamp) && !mtime.After(time.Now()) {
					stamp = mtime
				}
				if stamp.Before(started) {
					stamp = started
				}
				return stamp.UTC(), packageLogABI(f), nil
			}
		}
		if !complete && active {
			return time.Time{}, "", nil
		}
	} else if active {
		return time.Time{}, "", nil
	}
	if st.ModTime().Before(started) || st.ModTime().After(time.Now()) {
		return time.Time{}, "", nil
	}
	return st.ModTime().UTC(), packageLogABI(f), nil
}

// Poudriere records the build jail's uname -a at the start of each log.
// Some machine names cover more than one package architecture. In that case
// wait for the published package manifest to supply its exact ABI.
func packageLogABI(file io.ReaderAt) string {
	header, _ := io.ReadAll(io.NewSectionReader(file, 0, 4096))
	for line := range strings.SplitSeq(string(header), "\n") {
		info, ok := strings.CutPrefix(line, "building for: ")
		if !ok {
			continue
		}
		fields := strings.Fields(info)
		if len(fields) < 5 || fields[0] != "FreeBSD" {
			return ""
		}
		major, _, ok := strings.Cut(fields[2], ".")
		if !ok {
			return ""
		}
		arch := fields[len(fields)-1]
		switch arch {
		case "arm64":
			arch = "aarch64"
		case "amd64", "i386", "aarch64", "riscv64", "powerpc64le":
		default:
			return ""
		}
		return packageABI("FreeBSD:"+major+":"+arch, "")
	}
	return ""
}

type packageLogPosition struct{ built, failed packageLogCursor }

type packageLogHistory struct {
	cfg  BuilderConfig
	save func(context.Context, []BuildRecord) error
	// A nil cursor marks a completed run already imported in this process.
	runs map[string]*packageLogPosition
}

func (h *packageLogHistory) scan(ctx context.Context, status func() BuildStatus) error {
	base, err := os.OpenRoot(h.cfg.BaseFS)
	if err != nil {
		return err
	}
	defer base.Close()
	logs, err := base.OpenRoot(filepath.Join("data", "logs", "bulk", h.cfg.Jail+"-"+h.cfg.Ports))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer logs.Close()
	entries, err := fs.ReadDir(logs.FS(), ".")
	if err != nil {
		return err
	}
	build := status()
	activeName := ""
	if build.Active {
		activeName = build.Started.UTC().Format(buildNameFormat)
	}
	now := time.Now().UTC()
	// A build started before the retention window can still finish inside it.
	// Examine retained directories on the first pass; avoid revisiting finished
	// runs or collecting entries for log directories Poudriere later deletes.
	next := make(map[string]*packageLogPosition, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if !entry.IsDir() {
			continue
		}
		position, seen := h.runs[name]
		if seen && position == nil {
			next[name] = nil
			continue
		}
		started, err := time.Parse(buildNameFormat, name)
		if err != nil || started.After(now) {
			continue
		}
		dir, err := logs.OpenRoot(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if position == nil {
			position = &packageLogPosition{}
		}
		err = position.built.read(ctx, dir, started, "built", "succeeded", name == activeName, h.save)
		if err == nil {
			err = position.failed.read(ctx, dir, started, "failed", "failed", name == activeName, h.save)
		}
		dir.Close()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("read package results for %s: %v", name, err)
		}
		if name == activeName || err != nil {
			next[name] = position
		} else {
			next[name] = nil
		}
	}
	h.runs = next
	return nil
}

func (h *packageLogHistory) monitor(ctx context.Context, status func() BuildStatus, wake <-chan struct{}) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := h.scan(ctx, status); err != nil && ctx.Err() == nil {
			log.Printf("read Poudriere package history: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-tick.C:
		}
	}
}
