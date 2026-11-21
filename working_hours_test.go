package poudrine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkingHours(t *testing.T) {
	for _, tc := range []struct {
		hours, now, boundary string
		allowed              bool
	}{
		{"", "10:00", "", true},
		{"00:00-24:00", "23:59", "", true},
		{"23:15-07:00, 07:00-23:15", "12:00", "", true},
		{"00:00-10:00,11:00-17:00", "10:00", "11:00", false},
		{"00:00-10:00,11:00-17:00", "11:00", "17:00", true},
		{"23:15-07:00, 05:25-08:00", "23:30", "08:00", true},
		{"23:15-07:00, 05:25-08:00", "08:00", "23:15", false},
		{"22:00-24:00", "23:59", "00:00", true},
	} {
		t.Run(tc.hours+" at "+tc.now, func(t *testing.T) {
			clock, _ := time.Parse("15:04", tc.now)
			now := time.Date(2026, time.January, 10, clock.Hour(), clock.Minute(), 43, 1500, time.UTC)
			w, err := parseWorkingHours(tc.hours)
			if err != nil {
				t.Fatal(err)
			}
			ok, next := w.state(now)
			boundary := ""
			if !next.IsZero() {
				boundary = next.Format("15:04")
				if !next.After(now) || next.Sub(now) > 24*time.Hour || next.Second() != 0 || next.Nanosecond() != 0 {
					t.Fatalf("bad transition %v after %v", next, now)
				}
			}
			if ok != tc.allowed || boundary != tc.boundary {
				t.Fatalf("got %v, %v", ok, next)
			}
		})
	}
}

func TestWorkingHoursClockChanges(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		hours, now, next string
		allowed          bool
	}{
		// 02:00–02:59 do not occur on the spring transition.
		{"00:00-02:30", "2026-03-08T01:35:40-05:00", "2026-03-08T03:00:00-04:00", true},
		{"02:10-02:40", "2026-03-08T01:35:40-05:00", "2026-03-09T02:10:00-04:00", false},
		// Both occurrences of the autumn 01:30–02:00 window are eligible.
		{"01:30-02:00", "2026-11-01T01:45:20-04:00", "2026-11-01T01:00:00-05:00", true},
		{"01:30-02:00", "2026-11-01T01:15:00-05:00", "2026-11-01T01:30:00-05:00", false},
		{"01:30-02:00", "2026-11-01T01:35:20-05:00", "2026-11-01T02:00:00-05:00", true},
	} {
		w, err := parseWorkingHours(tc.hours)
		if err != nil {
			t.Fatal(err)
		}
		now, err := time.Parse(time.RFC3339, tc.now)
		if err != nil {
			t.Fatal(err)
		}
		ok, next := w.state(now.In(location))
		if ok != tc.allowed || next.Format(time.RFC3339) != tc.next {
			t.Errorf("%s from %s: %v, %s", tc.hours, tc.now, ok, next)
		}
	}
}

func TestInvalidWorkingHours(t *testing.T) {
	for _, value := range []string{"7am-5pm", "1:00-10:00", "24:00-01:00", "24:11-05:00", "09:60-13:00", "14:00-14:00", "02:00", "01:00-10:00,", "00:10-10:00,,11:00-13:00"} {
		_, err := decodeConfiguration([]byte(`{"working_hours":"`+value+`"}`), "")
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("accepted %q: %v", value, err)
		}
	}
}

func TestWorkingHoursLiveReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poudrine.json")
	writeConfig := func(raw string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(`{"web_tls":false}`)
	cfg, rev, err := loadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	o, err := cfg.options(path)
	if err != nil {
		t.Fatal(err)
	}
	o.configRevision = rev
	b, err := NewBuilder(o.builder, func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticks, ready := make(chan time.Time), make(chan *Builder)
	done := make(chan configTransition, 1)
	go func() { done <- watchConfiguration(ctx, "serve", o, ready, ticks) }()
	ready <- b
	writeConfig(`{"web_tls":false,"working_hours":"00:00-10:00"}`)
	ticks <- time.Now()
	// Synchronize with the watch loop after its configuration read.
	select {
	case ready <- b:
	case <-done:
		t.Fatal("working-hours edit restarted the build generation")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	hours, _ := parseWorkingHours("00:00-10:00")
	if b.currentWorkingHours() != hours {
		t.Fatal("worker still has the old schedule")
	}

	writeConfig(`{"web_tls":false,"working_hours":"00:00-10:00","jobs":2}`)
	select {
	case ticks <- time.Now():
	case <-done:
		t.Fatal("unexpected restart before the next settings check")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	change := <-done
	if change.next == nil || change.next.builder.Jobs != 2 || change.current.builder.WorkingHours != hours {
		t.Fatal("full settings change lost the live working hours")
	}
}
