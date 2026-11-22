package builder

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestRestartHoldResumesSameCompilerAndTransfersQueue(t *testing.T) {
	root := t.TempDir()
	cfg := testutil.WorkerConfig(root)
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0755); err != nil {
			t.Fatal(err)
		}
	}
	const setsid = "/usr/local/libexec/poudriere/setsid"
	if _, err := os.Stat(setsid); err != nil {
		t.Skip("Poudriere's setsid is unavailable")
	}
	write("poudriere", "#!/bin/sh\necho \"$$\" >> \"$FIXTURE/bulks\"\nexec "+setsid+" -w /bin/sh \"$FIXTURE/compile\"\n")
	write("compile", `echo "$$" >> "$FIXTURE/compilers"
while [ ! -e "$FIXTURE/done" ]; do
  echo progress >> "$FIXTURE/lines"
  sleep 0.03
done
`)
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	t.Setenv("FIXTURE", root)
	newWorker := func() *Builder {
		t.Helper()
		b, err := NewBuilder(cfg, func(context.Context) ([]string, error) {
			t.Error("a new build was admitted before re-exec")
			return nil, nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := newWorker()
	deadline := time.Now().Add(time.Hour).UTC()
	b.savedSchedule = BuildSchedule{Maintenance: deadline, LastOrigins: "www/firefox\n"}
	b.QueueRetry([]string{"www/firefox"})
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	var result error
	go func() { result = b.Run(ctx); close(finished) }()
	defer func() { cancel(); <-finished }()
	size := func() int64 {
		info, err := os.Stat(filepath.Join(root, "lines"))
		if err != nil {
			return 0
		}
		return info.Size()
	}
	awaitWorker(t, func() bool { return size() > 16 })
	started := b.Status().Started
	// A failed/canceled HTTP replacement must give its hold back to the
	// original worker, even when it already received the reaper's pause ACK.
	aborted, abort := context.WithCancel(ctx)
	undo, err := b.HoldForRestart(aborted)
	if err != nil {
		t.Fatal(err)
	}
	blockedAt := size()
	abort()
	awaitWorker(t, func() bool { return !b.Status().Paused && size() > blockedAt })
	undo()
	release, err := b.HoldForRestart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	before := size()
	awaitWorker(t, func() bool { return stoppedCompiler(ctx, filepath.Join(root, "compilers")) })
	if !b.Status().Paused || size() != before {
		t.Fatal("compiler continued through the acknowledged restart hold")
	}
	b.RequestBuild()
	b.QueueRetry([]string{"devel/protobuf"})
	b.SetWorkingHours(nextHourSchedule(t))
	b.ReexecAfterBuild()
	release()
	awaitWorker(t, func() bool {
		return !b.Status().WindowOpens.IsZero() && stoppedCompiler(ctx, filepath.Join(root, "compilers"))
	})
	if size() != before || !b.Status().Paused {
		t.Fatal("restart resumed the compiler outside working hours")
	}
	b.SetWorkingHours(config.WorkingHours{})
	awaitWorker(t, func() bool { return size() > before })
	journal, err := os.ReadFile(filepath.Join(cfg.ConfigDir, ".poudrine-pauses"))
	if err != nil || strings.Count(string(journal), started.Format(BuildNameFormat)+" ") != 2 {
		t.Fatalf("combined service/working-hours pause: %q, %v", journal, err)
	}
	write("done", "")
	select {
	case <-finished:
	case <-time.After(8 * time.Second):
		t.Fatal("worker did not yield to the updated supervisor after the build")
	}
	if !errors.Is(result, ErrWorkerReexec) {
		t.Fatalf("worker result: %v", result)
	}
	for _, name := range []string{"bulks", "compilers"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || len(strings.Fields(string(data))) != 1 {
			t.Fatalf("%s was relaunched: %q, %v", name, data, err)
		}
	}
	data, err := json.Marshal(b.Checkpoint())
	if err != nil {
		t.Fatal(err)
	}
	var cp WorkerCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatal(err)
	}
	next := newWorker()
	next.RestoreCheckpoint(&cp)
	if state := next.Status(); state.Outcome != "succeeded" || state.Active || !state.ManualPending ||
		state.RetryPending != 1 || !state.Started.Equal(started) || !next.savedSchedule.Maintenance.Equal(deadline) ||
		next.savedSchedule.LastOrigins != "www/firefox\n" || next.QueueRetry([]string{"devel/protobuf"}) {
		t.Fatalf("lost build or queue in handoff: %+v, %+v", state, cp)
	}
}

func TestRestartIdleWorkerOutsideHours(t *testing.T) {
	cfg := testutil.WorkerConfig(t.TempDir())
	cfg.WorkingHours = nextHourSchedule(t)
	b, err := NewBuilder(cfg, func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- b.Run(ctx) }()
	awaitWorker(t, func() bool { return !b.Status().WindowOpens.IsZero() })
	release, err := b.HoldForRestart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.ReexecAfterBuild()
	release()
	select {
	case err := <-finished:
		if !errors.Is(err, ErrWorkerReexec) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle update waited for the next working window")
	}
}
