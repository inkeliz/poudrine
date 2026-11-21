package poudrine

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Poudriere creates two sibling jails per builder (normal and networked).
// Include their descendants and never charge the master or host to a package.
func builderJails(master string, jobs []BuildJob, listing string) map[string]int {
	names := make(map[string]int, len(jobs))
	for i, job := range jobs {
		names[master+"-job-"+job.ID] = i
	}
	owners := make(map[string]int)
	for line := range strings.SplitSeq(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		jid, err := strconv.ParseUint(fields[0], 10, 31)
		if err != nil || jid == 0 {
			continue
		}
		name, _, _ := strings.Cut(fields[1], ".")
		if i, ok := names[strings.TrimSuffix(name, "-n")]; ok {
			owners[fields[0]] = i
		}
	}
	return owners
}

// FreeBSD ps reports a decaying CPU percentage (100% = one core) and RSS
// in KiB per process. Absent jails and unavailable snapshots stay unknown.
func setJobResources(jobs []BuildJob, owners map[string]int, processes string) {
	type usage struct {
		cpu    float64
		memory int64
		bad    bool
	}
	totals := make(map[int]usage, len(jobs))
	for _, i := range owners {
		totals[i] = usage{}
	}
	for line := range strings.SplitSeq(processes, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		i, ok := owners[fields[0]]
		if !ok {
			continue
		}
		total := totals[i]
		if len(fields) != 3 {
			total.bad = true
		} else {
			cpu, cpuErr := strconv.ParseFloat(fields[1], 64)
			rss, rssErr := strconv.ParseInt(fields[2], 10, 64)
			if cpuErr != nil || cpu < 0 || math.IsNaN(cpu) || math.IsInf((total.cpu+cpu)*10, 0) ||
				rssErr != nil || rss < 0 || rss > (math.MaxInt64-total.memory)/1024 {
				total.bad = true
			} else {
				total.cpu += cpu
				total.memory += rss * 1024
			}
		}
		totals[i] = total
	}
	for i, total := range totals {
		if !total.bad {
			jobs[i].CPUPercent = new(math.Round(total.cpu*10) / 10)
			jobs[i].MemoryBytes = new(total.memory)
		}
	}
}

func sampleJobResources(ctx context.Context, dir *os.Root, master string, jobs []BuildJob) {
	if len(jobs) == 0 || runtime.GOOS != "freebsd" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	jails, err := exec.CommandContext(ctx, "/usr/sbin/jls", "jid", "name").Output()
	if err != nil {
		return
	}
	owners := builderJails(master, jobs, string(jails))
	if len(owners) == 0 {
		return
	}
	ids := make([]string, 0, len(owners))
	for id := range owners {
		ids = append(ids, id)
	}
	ps := exec.CommandContext(ctx, "/bin/ps", "-J", strings.Join(ids, ","), "-o", "jid=", "-o", "pcpu=", "-o", "rss=")
	ps.Env = []string{"LC_ALL=C"}
	rows, err := ps.Output()
	if err != nil {
		var exit *exec.ExitError
		// ps exits 1 with no diagnostics when no processes match. An invalid
		// JID (e.g. jail removal between jls and ps) produces a diagnostic.
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(exit.Stderr) != 0 || len(rows) != 0 {
			return
		}
	}
	setJobResources(jobs, owners, string(rows))
	// A builder can finish one package and start another between reading its
	// status and invoking ps. Discard that ambiguous process sample.
	for i := range jobs {
		job := &jobs[i]
		if job.CPUPercent == nil {
			continue
		}
		fields := strings.SplitN(progressValue(dir, "status."+job.ID, 1024), ":", 4)
		if len(fields) < 3 || fields[1] != job.Origin || fields[2] != job.Package {
			job.CPUPercent, job.MemoryBytes = nil, nil
		}
	}
}
