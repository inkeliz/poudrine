package builder

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/packageinfo"
)

func (b *Builder) CurrentWorkingHours() config.WorkingHours {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hours
}

func (b *Builder) SetWorkingHours(hours config.WorkingHours) {
	b.mu.Lock()
	changed := b.hours != hours
	b.hours = hours
	b.mu.Unlock()
	if changed {
		b.wakeWindow()
	}
}

func (b *Builder) windowStatus(paused bool, opens time.Time, err error) {
	var msg string
	if err != nil {
		msg = BuildText(err.Error())
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.status.Paused == paused && b.status.WindowOpens.Equal(opens) && b.status.PauseError == msg {
		return
	}
	b.status.Paused, b.status.WindowOpens, b.status.PauseError = paused, opens, msg
	if b.ready {
		b.persistLocked()
	}
}

// Short wall-clock checks also handle timezone or clock changes while waiting.
func boundaryDelay(next time.Time) time.Duration {
	return min(30*time.Second, time.Until(next))
}

func (b *Builder) waitForWindow(ctx context.Context, idleBoundary bool) error {
	for ctx.Err() == nil {
		b.mu.Lock()
		allowed, next := b.hours.State(time.Now())
		held := b.restartHold != nil
		upgrade := idleBoundary && b.reexecPending && !held
		active := b.status.Active
		b.mu.Unlock()
		if upgrade {
			return ErrWorkerReexec
		}
		if allowed && !held {
			b.windowStatus(false, time.Time{}, nil)
			return nil
		}
		var tick <-chan time.Time
		var timer *time.Timer
		opens := time.Time{}
		if !allowed {
			opens = next
			timer = time.NewTimer(boundaryDelay(next))
			tick = timer.C
		}
		b.windowStatus(active, opens, nil)
		select {
		case <-ctx.Done():
		case <-b.hoursChanged:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
	return ctx.Err()
}

// Poudriere's nohang() uses epoch seconds for phase starts and log mtimes.
// Convert BOTH to elapsed build time within that watchdog. Other callers
// (including repository timestamps and build history) retain real time.
// POUDRIERE_ETC is set by poudriere -e after it clears its environment.
const poudrierePauseClockConfig = `
_poudrine_active_stamp() {
	local stamp="$1" elapsed=0 build start end
	while read -r build start end; do
		[ "${BUILDNAME-}" = "$build" ] || continue
		[ "$stamp" -gt "$start" ] || continue
		if [ "$stamp" -lt "$end" ]; then end="$stamp"; fi
		elapsed=$((elapsed + end - start))
	done < "${POUDRIERE_ETC}/.poudrine-pauses"
	echo "$((stamp - elapsed))"
}
clock() {
	if [ "$#" = 1 ] && [ "$1" = -epoch ] && [ -n "${cmd_timeout-}" ] &&
	    [ -n "${logfile-}" ] && [ -s "${POUDRIERE_ETC}/.poudrine-pauses" ]; then
		local value
		value=$(command clock "$@") || return
		_poudrine_active_stamp "$value"
	else
		command clock "$@"
	fi
}
stat() {
	if [ "$#" = 3 ] && [ "$1" = -f ] && [ "$2" = %m ] &&
	    [ "$3" = "${logfile-}" ] && [ -n "${cmd_timeout-}" ] &&
	    [ -s "${POUDRIERE_ETC}/.poudrine-pauses" ]; then
		local value
		value=$(command stat "$@") || return
		_poudrine_active_stamp "$value"
	else
		command stat "$@"
	fi
}
`

// Multiple retry clicks share the pending targeted run, separate from a
// regular manual build, which still processes the entire tracked list.
func (b *Builder) QueueRetry(origins []string) bool {
	// Reject the entire batch before mutating or persisting pending work.
	for _, origin := range origins {
		if len(origin) > 255 || !packageinfo.BuildOrigin.MatchString(origin) {
			return false
		}
	}
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

func (b *Builder) retryPackages(ctx context.Context, started time.Time, schedule *BuildSchedule, origins map[string]struct{}) {
	next, reason := schedule.Maintenance, "scheduled"
	if !schedule.retry.IsZero() {
		next, reason = schedule.retry, "retry"
	}
	file, err := os.CreateTemp(b.Cfg.ConfigDir, ".poudrine-retry-*")
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
