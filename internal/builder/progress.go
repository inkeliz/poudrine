package builder

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"golang.org/x/sys/unix"
)

// Poudriere includes fetched, ignored and skipped packages in its queue total.
type BuildProgress struct {
	Total   int `json:"total"`
	Built   int `json:"built"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	Ignored int `json:"ignored"`
	Fetched int `json:"fetched"`
}

type BuildJob struct {
	ID                   string   `json:"id"`
	Origin               string   `json:"origin"`
	Package              string   `json:"package"`
	Phase                string   `json:"phase"`
	ABI                  string   `json:"abi,omitempty"`
	ProgressPercent      *int     `json:"progress_percent,omitempty"`
	ElapsedSeconds       *int64   `json:"elapsed_seconds,omitempty"`
	ActiveElapsedSeconds *int64   `json:"active_elapsed_seconds,omitempty"`
	CPUPercent           *float64 `json:"cpu_percent,omitempty"`
	MemoryBytes          *int64   `json:"memory_bytes,omitempty"`
	LastBuildSeconds     *int64   `json:"last_build_seconds,omitempty"` // Filled by the HTTP service from its build history.
}

const BuildNameFormat = "2006-01-02_15h04m05s.000000000"

func (b *Builder) bulk(ctx context.Context, started time.Time, packagesFile string, fetchPackages bool) error {
	args := []string{"bulk", "-j", b.Cfg.Jail, "-p", b.Cfg.Ports}
	if len(b.Cfg.PackageOverlays) != 0 {
		if err := b.registerOverlay(ctx); err != nil {
			return err
		}
		args = append(args, "-O", config.OverlayTreeName)
	}
	// Pin the log directory to this invocation. "latest" can still point at
	// the previous build during setup, or change when another build starts.
	name := started.UTC().Format(BuildNameFormat)
	b.stage("building", "Preparing the package queue")
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		var logs jobLogs
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				b.updateProgress(ctx, name, &logs)
				return
			case <-tick.C:
				b.updateProgress(ctx, name, &logs)
			}
		}
	}()
	if fetchPackages {
		args = append(args, "-b", "latest")
	}
	args = append(args, "-f", packagesFile, "-B", name)
	_, err := b.command(ctx, false, args...)
	close(done)
	<-stopped
	return err
}

// These small files are written atomically by Poudriere's bset function.
// Treat inaccessible, incomplete, or version-specific counters as unknown.
func progressValue(dir *os.Root, file string, limit int64) string {
	f, err := dir.OpenFile(".poudriere."+file, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func packageProgress(dir *os.Root) *BuildProgress {
	var p BuildProgress
	for _, counter := range []struct {
		name  string
		value *int
	}{
		{"queued", &p.Total}, {"built", &p.Built}, {"failed", &p.Failed},
		{"skipped", &p.Skipped}, {"ignored", &p.Ignored}, {"fetched", &p.Fetched},
	} {
		count, err := strconv.ParseUint(progressValue(dir, "stats_"+counter.name, 32), 10, 31)
		if err != nil {
			return nil
		}
		*counter.value = int(count)
	}
	if int64(p.Built)+int64(p.Failed)+int64(p.Skipped)+int64(p.Ignored)+int64(p.Fetched) > int64(p.Total) {
		return nil // The queue is being recalculated between counter writes.
	}
	return &p
}

var jobPhase = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_/-]*$`)

func runningJobs(dir *os.Root, now int64, logs *jobLogs) ([]BuildJob, int64) {
	var jobs []BuildJob
	var lastSlot int64
	next := make(jobLogs)
	for id := range strings.FieldsSeq(progressValue(dir, "builders", 1<<20)) {
		slot, err := strconv.ParseUint(id, 10, 32)
		if err != nil {
			continue
		}
		lastSlot = max(lastSlot, int64(slot))
		// Poudriere reports phase:origin:package:job_start:phase_start.
		// The last two fields use monotonic seconds since boot.
		status := progressValue(dir, "status."+id, 1024)
		fields := strings.Split(status, ":")
		if len(fields) < 3 || !jobPhase.MatchString(fields[0]) || !packageinfo.BuildOrigin.MatchString(fields[1]) || !packageinfo.JobPackage.MatchString(fields[2]) {
			continue
		}
		job := BuildJob{ID: id, Origin: fields[1], Package: fields[2], Phase: fields[0]}
		if len(fields) >= 4 {
			start, err := strconv.ParseInt(fields[3], 10, 64)
			if err == nil && start >= 0 && now >= start {
				job.ElapsedSeconds = new(now - start)
			}
		}
		key := id + ":" + job.Package
		if len(fields) >= 4 {
			key += ":" + fields[3]
		}
		cursor := (*logs)[key]
		job.ABI, job.ProgressPercent = cursor.sample(dir, job.Package, job.Phase == "build")
		next[key] = cursor
		jobs = append(jobs, job)
	}
	*logs = next
	return jobs, lastSlot
}

var bulkPhases = map[string]string{
	"loading_moved":         "Checking for moved ports",
	"gatheringportvars":     "Reading ports metadata",
	"computingdeps":         "Resolving package dependencies",
	"computingrdeps":        "Resolving reverse dependencies",
	"trimming_ignore":       "Checking ignored ports",
	"sanity":                "Checking the package repository",
	"pkgqueue_sanity_check": "Checking the build queue",
	"load_priorities":       "Prioritizing the build queue",
	"fetching_packages":     "Fetching available packages",
	"fetched_package_logs":  "Recording fetched packages",
	"building":              "Building tracked ports",
	"starting_jobs":         "Starting build jobs",
	"starting_builders":     "Starting package builders",
	"parallel_build":        "Building packages",
	"stopping_jobs":         "Stopping package builders",
	"updating_stats":        "Updating package counts",
	"clean_restricted":      "Cleaning restricted packages",
	"pkgrepo":               "Creating package repository",
	"committing":            "Publishing the package repository",
}

func (b *Builder) updateProgress(ctx context.Context, buildname string, logs *jobLogs) {
	base, err := os.OpenRoot(b.Cfg.BaseFS)
	if err != nil {
		return
	}
	defer base.Close()
	master := b.Cfg.Jail + "-" + b.Cfg.Ports
	dir, err := base.OpenRoot(filepath.Join("data", "logs", "bulk", master, buildname))
	if err != nil {
		return
	}
	defer dir.Close()
	progress := packageProgress(dir)
	now := monotonicSecond()
	jobs, lastSlot := runningJobs(dir, now, logs)
	sampleJobResources(ctx, dir, master, jobs)
	phase, _, _ := strings.Cut(progressValue(dir, "status", 128), ":")
	message := bulkPhases[phase]
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range jobs {
		job := &jobs[i]
		job.ActiveElapsedSeconds = b.jobClock.activeSeconds(now, job.ElapsedSeconds)
	}
	changed := false
	// Poudriere numbers builders from one and may allocate fewer for a
	// short queue. Keep the configured capacity and any larger live slot.
	if lastSlot > max(b.status.BuilderCapacity, int64(b.Cfg.Jobs)) {
		b.status.BuilderCapacity = lastSlot
		changed = true
	}
	if progress != nil && (b.status.Progress == nil || *progress != *b.status.Progress) {
		b.status.Progress = progress
		changed = true
	}
	if message != "" && b.status.Message != message {
		b.status.Message = message
		changed = true
	}
	if !reflect.DeepEqual(b.status.Jobs, jobs) {
		b.status.Jobs = jobs
		changed = true
	}
	if changed {
		b.persistLocked()
	}
}

func monotonicSecond() int64 {
	var stamp unix.Timespec
	if unix.ClockGettime(unix.CLOCK_MONOTONIC, &stamp) != nil {
		return -1
	}
	return int64(stamp.Sec)
}

// Times are seconds on Poudriere's job clock. Each interval records a
// confirmed process stop; an open interval ends at the current sample.
type jobPause struct{ start, end int64 }

type jobClock struct {
	pauses      []jobPause
	unavailable bool
}

func (c *jobClock) mark(paused bool, now int64) {
	if now < 0 {
		c.unavailable = true
	} else if paused {
		c.pauses = append(c.pauses, jobPause{start: now, end: -1})
	} else if n := len(c.pauses); n > 0 && c.pauses[n-1].end < 0 {
		c.pauses[n-1].end = now
	}
}

func (c *jobClock) activeSeconds(now int64, elapsed *int64) *int64 {
	if elapsed == nil || c.unavailable {
		return nil
	}
	active := *elapsed
	start := now - active
	for _, pause := range c.pauses {
		end := now
		if pause.end >= 0 {
			end = min(pause.end, now)
		}
		active -= max(0, end-max(start, pause.start))
	}
	return &active
}
