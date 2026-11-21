package poudrine

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
)

// Multiple retry clicks share the pending targeted run, separate from a
// regular manual build, which still processes the entire tracked list.
func (b *Builder) queueRetry(origins []string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.retryOrigins == nil {
		b.retryOrigins = make(map[string]struct{}, len(origins))
	}
	before := len(b.retryOrigins)
	for _, origin := range origins {
		b.retryOrigins[origin] = struct{}{}
	}
	if len(b.retryOrigins) == before {
		return false
	}
	if b.ready {
		b.persistLocked()
	}
	b.Wake()
	return true
}

func (b *Builder) retryPackages(ctx context.Context, started time.Time, schedule *buildSchedule, origins map[string]struct{}) {
	next, reason := schedule.maintenance, "scheduled"
	if !schedule.retry.IsZero() {
		next, reason = schedule.retry, "retry"
	}
	file, err := os.CreateTemp(b.cfg.ConfigDir, ".poudrine-retry-*")
	if err == nil {
		defer os.Remove(file.Name())
		_, err = file.WriteString(strings.Join(slices.Sorted(maps.Keys(origins)), "\n") + "\n")
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	bulk := err == nil && ctx.Err() == nil
	if bulk {
		// A fresh build name reconsiders failed and skipped packages. Compile
		// missing packages locally and preserve archives since built successfully.
		err = b.bulk(ctx, started, file.Name(), false)
	}
	b.complete(ctx, err, bulk, time.Now().UTC(), next, reason)
	b.Wake() // Recheck any coalesced full build or expired maintenance deadline.
}
