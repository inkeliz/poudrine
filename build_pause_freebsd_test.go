package poudrine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func nextHourSchedule(t *testing.T) workingHours {
	t.Helper()
	// Pick a short window well away from the current wall clock.
	start := (time.Now().Hour() + 4) % 24
	hours, err := parseWorkingHours(fmt.Sprintf("%02d:00-%02d:01", start, start))
	if err != nil {
		t.Fatal(err)
	}
	return hours
}

func TestPauseRunningBuildTree(t *testing.T) {
	root := t.TempDir()
	setsid := "/usr/local/libexec/poudriere/setsid"
	if _, err := os.Stat(setsid); err != nil {
		t.Skip("Poudriere's setsid utility is unavailable")
	}
	cfg := workerTestConfig(root)
	if err := os.Mkdir(cfg.ConfigDir, 0755); err != nil {
		t.Fatal(err)
	}
	b, err := NewBuilder(cfg, func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Poudriere starts configure/build jobs in new sessions. Check that an
	// actual descendant in such a session keeps its PID and progress.
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0755); err != nil {
			t.Fatal(err)
		}
	}
	write("watchdog.conf", poudrierePauseClockConfig)
	write("poudriere", `#!/usr/local/libexec/poudriere/sh
PATH=/usr/local/libexec/poudriere:$PATH
POUDRIERE_ETC="$FIXTURE/config"
BUILDNAME=test-job
. "$FIXTURE/watchdog.conf"
. /usr/local/share/poudriere/include/util.sh
. /usr/local/share/poudriere/include/parallel.sh
echo "$$" >> "$FIXTURE/bulks"
: > "$FIXTURE/job.log"
nohang 3 10 "$FIXTURE/job.log" "$FIXTURE/nohang.pid" setsid -w /bin/sh "$FIXTURE/compile"
`)
	write("compile", `#!/bin/sh
echo "$$" >> "$FIXTURE/compiler"
while [ ! -e "$FIXTURE/finish" ]; do
	echo work >> "$FIXTURE/progress"
	sleep 0.05
done
`)
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	t.Setenv("FIXTURE", root)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	finished := false
	t.Cleanup(func() {
		os.WriteFile(filepath.Join(root, "finish"), nil, 0644)
		cancel()
		if !finished {
			<-done
		}
	})
	var stderr bytes.Buffer
	go func() { done <- b.runWorkCommand(ctx, []string{"bulk", "-B", "test-job"}, io.Discard, &stderr) }()
	size := func() int64 {
		s, err := os.Stat(filepath.Join(root, "progress"))
		if err != nil {
			return 0
		}
		return s.Size()
	}
	awaitWorker(t, func() bool { return size() >= 10 })
	for i := 0; i < 2; i++ {
		b.SetWorkingHours(nextHourSchedule(t))
		awaitWorker(t, func() bool { return b.Status().Paused })
		before := size()
		if i == 0 {
			// The installed watchdog allows three running seconds. Leave its
			// entire subtree suspended past that deadline, and after resuming
			// let the watchdog's real one-second poll run at least once.
			time.Sleep(5 * time.Second)
		} else {
			time.Sleep(100 * time.Millisecond)
		}
		if size() != before || b.Status().WindowOpens.IsZero() {
			t.Fatal("compiler advanced while paused", b.Status())
		}
		b.SetWorkingHours(workingHours{})
		awaitWorker(t, func() bool { return !b.Status().Paused && size() > before })
		if i == 0 {
			time.Sleep(1100 * time.Millisecond)
		}
	}
	journal, err := os.ReadFile(filepath.Join(cfg.ConfigDir, ".poudrine-pauses"))
	if err != nil || strings.Count(string(journal), "test-job ") != 2 {
		t.Fatalf("pause journal: %s, %v", journal, err)
	}
	if err := os.WriteFile(filepath.Join(root, "finish"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	err = <-done
	finished = true
	if err != nil {
		t.Fatalf("completed build: %v, %s", err, stderr.String())
	}
	for _, name := range []string{"compiler", "bulks"} {
		buf, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || len(strings.Fields(string(buf))) != 1 {
			t.Fatalf("%s restarted: %q %v", name, buf, err)
		}
	}
}

func TestClosedWindowQueuesManualRun(t *testing.T) {
	root := t.TempDir()
	cfg := workerTestConfig(root)
	cfg.WorkingHours = nextHourSchedule(t)
	if err := os.WriteFile(filepath.Join(root, "poudriere"), []byte("#!/bin/sh\necho called >> \"$FIXTURE/calls\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	t.Setenv("FIXTURE", root)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b, err := NewBuilder(cfg, func(context.Context) ([]string, error) { return []string{"devel/fixture"}, nil }, func(_ context.Context, err error) {
		if err != nil {
			t.Errorf("worker run: %v", err)
		}
		cancel()
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	defer func() { cancel(); <-done }()
	awaitWorker(t, func() bool { return !b.Status().WindowOpens.IsZero() })
	if !b.RequestBuild() || b.RequestBuild() {
		t.Fatal("manual queue changed outside working hours")
	}
	time.Sleep(100 * time.Millisecond)
	status := b.Status()
	if status.Active || !status.ManualPending || status.NextRun.IsZero() || status.NextReason != "working_hours" {
		t.Fatal(status)
	}
	if _, err := os.Stat(filepath.Join(root, "calls")); !os.IsNotExist(err) {
		t.Fatal("started Poudriere outside working hours", err)
	}
	b.SetWorkingHours(workingHours{})
	awaitWorker(t, func() bool { return !b.Status().Finished.IsZero() })
	if s := b.Status(); s.Trigger != "manual" || s.ManualPending || s.Outcome != "succeeded" {
		t.Fatal(s)
	}
}

func TestPackageRetriesWaitForWindowAndActiveBuild(t *testing.T) {
	root := t.TempDir()
	cfg := workerTestConfig(root)
	cfg.WorkingHours = nextHourSchedule(t)
	const script = `#!/bin/sh
set -eu
while [ "$1" != "-f" ]; do shift; done
read -r origin < "$2"
echo "$origin" >> "$RETRY_TEST/calls"
if [ "$origin" = "devel/abseil" ]; then
  while [ ! -e "$RETRY_TEST/release" ]; do sleep 0.05; done
fi
`
	if err := os.WriteFile(filepath.Join(root, "poudriere"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	t.Setenv("RETRY_TEST", root)
	b, err := NewBuilder(cfg, func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	completed := make(chan BuildStatus, 2)
	b.onBuildFinished = func(_ context.Context, status BuildStatus) {
		completed <- status
		if status.RetryPending == 0 {
			cancel()
		}
	}
	b.queueRetry([]string{"devel/abseil"})
	stopped := make(chan error, 1)
	go func() { stopped <- b.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	awaitWorker(t, func() bool { return !b.Status().WindowOpens.IsZero() })
	if status := b.Status(); status.Active || status.RetryPending != 1 || status.NextReason != "working_hours" {
		t.Fatalf("retry queued outside working hours: %+v", status)
	}
	calls := filepath.Join(root, "calls")
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("retry started outside working hours: %v", err)
	}
	b.SetWorkingHours(workingHours{})
	awaitWorker(t, func() bool { data, _ := os.ReadFile(calls); return string(data) == "devel/abseil\n" })
	first := b.Status()
	second := []string{"devel/protobuf"}
	if !b.queueRetry(second) || b.queueRetry(second) {
		t.Fatal("expected a single follow-up retry")
	}
	if s := b.Status(); !s.Active || !s.Started.Equal(first.Started) || s.ManualPending || s.RetryPending != 1 {
		t.Fatalf("queueing another retry interrupted the active run: %+v", s)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	awaitWorker(t, func() bool { return len(completed) == 2 })
	one, two := <-completed, <-completed
	if one.Trigger != "retry" || two.Trigger != "retry" || one.Outcome != "succeeded" || two.Outcome != "succeeded" ||
		two.Started.Before(one.Finished) || one.RetryPending != 1 || two.RetryPending != 0 {
		t.Fatalf("expected sequential completed retries: %+v %+v", one, two)
	}
	if data, err := os.ReadFile(calls); err != nil || string(data) != "devel/abseil\ndevel/protobuf\n" {
		t.Fatalf("unexpected Poudriere operations %q: %v", data, err)
	}
}

func TestPoudriereWatchdogClock(t *testing.T) {
	sh := "/usr/local/libexec/poudriere/sh"
	if _, err := os.Stat(sh); err != nil {
		t.Skip("requires Poudriere's bundled shell")
	}
	now := time.Now().Unix()
	root := t.TempDir()
	path := filepath.Join(root, "log")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(now-400, 0)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	pauses := fmt.Sprintf("previous %d %d\ncurrent %d %d\ncurrent %d %d\n", now-1200, now-1000, now-700, now-500, now-450, now-350)
	if err := os.WriteFile(filepath.Join(root, ".poudrine-pauses"), []byte(pauses), 0600); err != nil {
		t.Fatal(err)
	}
	script := poudrierePauseClockConfig + `
BUILDNAME=current
logfile="$POUDRIERE_ETC/log"
ordinary=$(stat -f %m "$logfile")
outside=$(clock -epoch)
watch() {
	local cmd_timeout=7200 log_timeout=120 logfile="$1"
	stat -f %m "$logfile"
	clock -epoch
}
echo "$ordinary"
echo "$outside"
watch "$logfile"
`
	cmd := exec.Command(sh, "-c", script)
	cmd.Env = append(os.Environ(), "POUDRIERE_ETC="+root, "PATH=/usr/local/libexec/poudriere:"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shell: %v %s", err, out)
	}
	values := strings.Fields(string(out))
	if len(values) != 4 {
		t.Fatalf("shell output %s", out)
	}
	var v []int64
	for _, s := range values {
		x, e := strconv.ParseInt(s, 10, 64)
		if e != nil {
			t.Fatal(e)
		}
		v = append(v, x)
	}
	if v[0] != mtime.Unix() || v[1] < now || v[2] != mtime.Unix()-250 || v[3] < now-300 || v[3] > time.Now().Unix()-300 {
		t.Fatalf("wall/active timestamps %v for now %d", v, now)
	}
}

func TestShutdownWhileBuildPaused(t *testing.T) {
	root := t.TempDir()
	cfg := workerTestConfig(root)
	b, err := NewBuilder(cfg, func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	const script = `#!/bin/sh
trap 'echo cleanup > "$FIXTURE/cleaned"; exit 0' TERM
echo ready > "$FIXTURE/started"
while :; do sleep 0.1; done
`
	if err := os.WriteFile(filepath.Join(root, "poudriere"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	t.Setenv("FIXTURE", root)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.runWorkCommand(ctx, []string{"bulk", "-B", "shutdown"}, io.Discard, io.Discard) }()
	awaitWorker(t, func() bool { _, e := os.Stat(filepath.Join(root, "started")); return e == nil })
	b.SetWorkingHours(nextHourSchedule(t))
	awaitWorker(t, func() bool { return b.Status().Paused })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("paused Poudriere could not shut down")
	}
	if _, err := os.Stat(filepath.Join(root, "cleaned")); err != nil {
		t.Fatal("Poudriere did not run its cleanup handler", err)
	}
}
