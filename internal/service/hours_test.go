package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/config"
)

func TestWorkingHoursLiveReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poudrine.json")
	writeConfig := func(raw string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(`{"web_tls":false}`)
	cfg, rev, err := config.LoadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	o, err := cfg.Options(path)
	if err != nil {
		t.Fatal(err)
	}
	o.ConfigRevision = rev
	b, err := builder.NewBuilder(o.Builder, func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticks, ready := make(chan time.Time), make(chan *builder.Builder)
	done := make(chan configTransition, 1)
	go func() { done <- watchConfiguration(ctx, "serve", o, ready, ticks, nil) }()
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
	hours, _ := config.ParseWorkingHours("00:00-10:00")
	if b.CurrentWorkingHours() != hours {
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
	if change.next == nil || change.next.Builder.Jobs != 2 || change.current.Builder.WorkingHours != hours {
		t.Fatal("full settings change lost the live working hours")
	}
}
