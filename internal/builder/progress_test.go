package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestManualBuildQueueAndLiveProgress(t *testing.T) {
	root := t.TempDir()
	cfg := testutil.WorkerConfig(root)
	command := `#!/bin/sh
shift 3
echo "$*" >> "$WORK_TEST/calls"
case "$1 $2" in
"jail -l") echo latest ;;
"ports -l") echo main ;;
"bulk -j")
  for name do :; done
  dir="$WORK_TEST/base/data/logs/bulk/latest-main/$name"
  mkdir -p "$dir"
  echo 9 > "$dir/.poudriere.stats_queued"
  echo 1 > "$dir/.poudriere.stats_built"
  echo 1 > "$dir/.poudriere.stats_fetched"
  for n in failed ignored skipped; do echo 0 > "$dir/.poudriere.stats_$n"; done
  echo parallel_build: > "$dir/.poudriere.status"
  echo "01 02" > "$dir/.poudriere.builders"
  echo idle: > "$dir/.poudriere.status.01"
  echo build:ftp/curl:curl-8.0:2:3 > "$dir/.poudriere.status.02"
  while [ ! -f "$WORK_TEST/release" ]; do sleep .05; done
  echo 8 > "$dir/.poudriere.stats_built"
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "poudriere"), []byte(command), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	t.Setenv("WORK_TEST", root)
	ctx, cancel := context.WithCancel(context.Background())
	b, err := NewBuilder(cfg, func(context.Context) ([]string, error) { return []string{"ftp/curl"}, nil }, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	completed := make(chan BuildStatus, 10)
	b.OnBuildFinished = func(_ context.Context, s BuildStatus) { completed <- s }
	stopped := make(chan error, 1)
	go func() { stopped <- b.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-stopped; !errors.Is(err, context.Canceled) {
			t.Errorf("worker stop: %v", err)
		}
	}()
	awaitWorker(t, func() bool {
		s, _ := ReadBuildStatus(cfg.StatusFile)
		return s.Active && s.Trigger == "scheduled" && len(s.Jobs) == 1 && s.Progress != nil
	})
	active := b.Status()
	if active.BuilderCapacity != int64(cfg.Jobs) {
		t.Fatalf("two allocated slots reduced the configured capacity to %d, want %d", active.BuilderCapacity, cfg.Jobs)
	}
	if active.Jobs[0].Package != "curl-8.0" || active.Message != "Building packages" || active.Progress.Total != 9 || active.Progress.Built != 1 || active.Origins == nil || *active.Origins != 1 || active.NextDelaySeconds != cfg.Interval.Seconds() {
		t.Fatalf("unexpected progress %+v", active)
	}
	if !b.RequestBuild() || b.RequestBuild() {
		t.Fatal("expected one pending manual build")
	}
	queued, err := ReadBuildStatus(cfg.StatusFile)
	if err != nil || !queued.Active || !queued.ManualPending || queued.NextReason != "manual" || !queued.NextRun.IsZero() || queued.NextDelaySeconds != 0 {
		t.Fatalf("queued: %+v, %v", queued, err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	next := func() BuildStatus {
		t.Helper()
		select {
		case s := <-completed:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("build did not finish")
			return BuildStatus{}
		}
	}
	first, manual := next(), next()
	if first.Trigger != "scheduled" || manual.Trigger != "manual" || manual.Active || manual.ManualPending ||
		manual.Outcome != "succeeded" || manual.Progress.Built != 8 || !manual.NextRun.Equal(first.Finished.Add(cfg.Interval)) {
		t.Fatalf("scheduled/forced runs: %+v %+v", first, manual)
	}
	if !b.RequestBuild() {
		t.Fatal("idle manual request not accepted")
	}
	idleForced := next()
	if idleForced.Trigger != "manual" || !idleForced.NextRun.Equal(manual.NextRun) {
		t.Fatalf("forced idle run: %+v", idleForced)
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil || strings.Count(string(calls), "bulk -j") != 3 || strings.Count(string(calls), "ports -u") != 3 {
		t.Fatalf("expected exactly three updated builds: %s %v", calls, err)
	}
}

func TestManualBuildBypassesRetryAndEmptyMaintenanceDoesNotCount(t *testing.T) {
	root := t.TempDir()
	cfg := testutil.WorkerConfig(root)
	if err := os.WriteFile(filepath.Join(root, "poudriere"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := true
	cycles := make(chan struct{}, 3)
	lookup := make(chan struct{})
	releaseLookup := make(chan struct{})
	b, err := NewBuilder(cfg, func(ctx context.Context) ([]string, error) {
		if failed {
			failed = false
			close(lookup)
			select {
			case <-releaseLookup:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return nil, errors.New("inventory unavailable")
		}
		return nil, nil
	}, func(context.Context, error) { cycles <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	// A request can arrive while the supervisor is installing prerequisites.
	if !b.RequestBuild() || b.RequestBuild() || b.Status().Stage != "waiting" {
		t.Fatal("startup request was not queued")
	}
	if _, err := os.Stat(cfg.StatusFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("request tried to publish status before worker setup")
	}
	b.OnBuildFinished = func(context.Context, BuildStatus) { t.Error("counted maintenance without a bulk invocation") }
	stopped := make(chan error, 1)
	go func() { stopped <- b.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	select {
	case <-lookup:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not read tracked origins")
	}
	if !b.Status().ManualPending || b.RequestBuild() {
		t.Fatal("lost the pending request while reading tracked origins")
	}
	close(releaseLookup)
	wait := func() {
		select {
		case <-cycles:
		case <-time.After(5 * time.Second):
			t.Fatal("worker missed force/retry")
		}
	}
	wait()
	s := b.Status()
	if s.Stage != "error" || s.NextReason != "retry" || s.NextRun.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("expected visible retry deadline, got %+v", s)
	}
	b.RequestBuild()
	wait()
	s = b.Status()
	if s.Stage != "idle" || s.Trigger != "manual" || s.Origins == nil || *s.Origins != 0 || s.NextReason != "scheduled" {
		t.Fatalf("manual retry: %+v", s)
	}
}

func TestBulkFailureAndGracefulInterruption(t *testing.T) {
	root := t.TempDir()
	cfg := testutil.WorkerConfig(root)
	const script = `#!/bin/sh
shift 3
if [ "$1" = bulk ]; then
  if [ ! -f "$WORK_TEST/first" ]; then
    touch "$WORK_TEST/first"
    exit 1
  fi
  touch "$WORK_TEST/second"
  exec sleep 60
fi
`
	if err := os.WriteFile(filepath.Join(root, "poudriere"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	t.Setenv("WORK_TEST", root)
	b, err := NewBuilder(cfg, func(context.Context) ([]string, error) { return []string{"ftp/curl"}, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	completions := make(chan BuildStatus, 2)
	b.OnBuildFinished = func(_ context.Context, s BuildStatus) {
		onDisk, err := ReadBuildStatus(cfg.StatusFile)
		if err != nil || onDisk.Active || !onDisk.Finished.Equal(s.Finished) || onDisk.Outcome != s.Outcome {
			t.Errorf("completion published before final status: %+v %v", onDisk, err)
		}
		completions <- s
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- b.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	awaitWorker(t, func() bool { return len(completions) > 0 })
	if s := <-completions; s.Outcome != "failed" || s.NextReason != "retry" {
		t.Fatalf("bulk failure: %+v", s)
	}
	b.RequestBuild()
	awaitWorker(t, func() bool {
		_, err := os.Stat(filepath.Join(root, "second"))
		return err == nil
	})
	cancel()
	awaitWorker(t, func() bool { return len(completions) > 0 })
	if s := <-completions; s.Outcome != "interrupted" || s.Active || s.NextReason != "" || !s.NextRun.IsZero() {
		t.Fatalf("bulk interruption: %+v", s)
	}
}

func TestLegacyWorkerStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	for _, stage := range []string{"building", "preparing", "ports", "jail", "idle", "error"} {
		if err := os.WriteFile(path, []byte(`{"stage":"`+stage+`"}`), 0644); err != nil {
			t.Fatal(err)
		}
		s, err := ReadBuildStatus(path)
		active := stage != "idle" && stage != "error"
		if err != nil || s.Active != active || s.Origins != nil || !s.NextRun.IsZero() || s.NextDelaySeconds != 0 || s.NextReason != "" {
			t.Fatalf("reading legacy %s: %+v %v", stage, s, err)
		}
	}
}
