package poudrine

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"
)

var errWorkerReexec = errors.New("worker ready for service upgrade")

// Only the command's control loop can acknowledge that its reaper has
// stopped every descendant. Between commands, holding the launch gate is
// sufficient. The worker keeps the command's pipes and Wait alive throughout.
type buildRestartHold struct {
	ready chan error
}

func (h *buildRestartHold) acknowledge(err error) {
	if h != nil {
		select {
		case h.ready <- err:
		default:
		}
	}
}

func (b *Builder) wakeWindow() {
	select {
	case b.hoursChanged <- struct{}{}:
	default:
	}
	b.Wake()
}

func (b *Builder) holdForRestart(ctx context.Context) (func(), error) {
	b.mu.Lock()
	if b.restartHold != nil {
		b.mu.Unlock()
		return nil, errors.New("service restart already in progress")
	}
	hold := &buildRestartHold{ready: make(chan error, 1)}
	b.restartHold = hold
	if !b.commandRunning {
		hold.acknowledge(nil)
	}
	b.mu.Unlock()
	b.wakeWindow()
	release := func() {
		b.mu.Lock()
		if b.restartHold == hold {
			b.restartHold = nil
		}
		b.mu.Unlock()
		b.wakeWindow()
	}
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-hold.ready:
		if err == nil {
			err = ctx.Err()
		}
	}
	if err != nil {
		release()
		return nil, err
	}
	stop := context.AfterFunc(ctx, release)
	return func() {
		stop()
		release()
	}, nil
}

// Claim the launch while checking the same mutex as holdForRestart.
// A hold racing a claimed command waits for its reaper acknowledgement.
func (b *Builder) startWork(ctx context.Context) error {
	for {
		if err := b.waitForWindow(ctx, false); err != nil {
			return err
		}
		b.mu.Lock()
		allowed, _ := b.hours.state(time.Now())
		if allowed && b.restartHold == nil {
			b.commandRunning = true
			b.mu.Unlock()
			return nil
		}
		b.mu.Unlock()
	}
}

func (b *Builder) endWork() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commandRunning = false
	b.restartHold.acknowledge(nil)
}

func (b *Builder) reexecAfterBuild() {
	b.mu.Lock()
	b.reexecPending = true
	b.mu.Unlock()
	b.wakeWindow()
}

func (b *Builder) reserveConfigChange() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.status.Active || b.restartHold != nil || b.reexecPending {
		return false
	}
	// The watcher now cancels this idle generation. A concurrent tracked
	// origin or admin request must not launch Poudriere in the interval.
	b.restartHold = &buildRestartHold{ready: make(chan error, 1)}
	return true
}

// The supervisor transfers this across its own exec only after Run returned
// at an idle boundary. Keep the internal schedule separately from the
// presentation fields, which also describe working windows and manual builds.
type workerCheckpoint struct {
	Status           BuildStatus
	RetryOrigins     []string
	Maintenance      time.Time
	Retry            time.Time
	RetryMaintenance bool
	LastOrigins      string
}

func (b *Builder) checkpoint() *workerCheckpoint {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &workerCheckpoint{
		Status: b.status, RetryOrigins: slices.Sorted(maps.Keys(b.retryOrigins)),
		Maintenance: b.savedSchedule.maintenance, Retry: b.savedSchedule.retry,
		RetryMaintenance: b.savedSchedule.retryMaintenance, LastOrigins: b.savedSchedule.lastOrigins,
	}
}

func (b *Builder) restoreCheckpoint(cp *workerCheckpoint) {
	if cp == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status = cp.Status
	b.retryOrigins = make(map[string]struct{}, len(cp.RetryOrigins))
	for _, origin := range cp.RetryOrigins {
		b.retryOrigins[origin] = struct{}{}
	}
	b.savedSchedule = buildSchedule{
		maintenance: cp.Maintenance, retry: cp.Retry, retryMaintenance: cp.RetryMaintenance, lastOrigins: cp.LastOrigins,
	}
}
