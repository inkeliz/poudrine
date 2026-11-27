package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/public"
	"github.com/inkeliz/poudrine/internal/statusproto"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestPublicStatsPublisher(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	if err := db.RecordBuilds(t.Context(), []database.BuildRecord{
		{Package: "test-1", Started: day.Add(-time.Hour), Finished: day, Outcome: "succeeded"},
		{Package: "test-1", Started: day.Add(-25 * time.Hour), Finished: day.AddDate(0, 0, -1), Outcome: "succeeded"},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "status.json")
	file, err := public.OpenStatsWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cat := &catalog.Catalog{}
	cat.Current.Store(&catalog.Snapshot{
		ByPath: map[string]packageinfo.Package{"All/test-1.pkg": {Name: "test", Version: "1", Local: true}},
		Ahead:  1, LocalBuilds: 1,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		monitorPublicStats(ctx, cat, *db, file)
	}()
	defer func() { cancel(); <-stopped }()
	var counts statusproto.Counts
	if err := testutil.Wait(ctx, func() bool {
		raw, _ := os.ReadFile(path)
		var ok bool
		counts, ok = statusproto.Parse(raw)
		return ok
	}); err != nil {
		t.Fatal("waiting for public snapshot", err)
	}
	if counts.Packages != 1 || counts.Ahead != 1 || counts.LocalBuilds == nil || *counts.LocalBuilds != 1 ||
		counts.Compilations == nil || counts.Compilations.Last7Days != 2 || counts.Compilations.Yesterday != 1 {
		t.Fatalf("unexpected public counts: %+v (compilations %+v)", counts, counts.Compilations)
	}
	wantToday := int64(0)
	if counts.Compilations.UTCDate == statusproto.UTCDate(day) {
		wantToday = 1
	}
	if counts.Compilations.Today != wantToday {
		t.Fatalf("unexpected number of compilations today: %+v", counts.Compilations)
	}
}
