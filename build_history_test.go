package poudrine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func historyTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestBuildBucketCalendar(t *testing.T) {
	for _, tc := range []struct {
		interval, now, first, last, end string
		count                           int
	}{
		{"hour", "2024-02-29T23:25:45-05:00", "2024-02-29T05:00:00Z", "2024-03-01T04:00:00Z", "2024-03-01T05:00:00Z", 24},
		{"day", "2024-03-01T00:00:00Z", "2024-02-01T00:00:00Z", "2024-03-01T00:00:00Z", "2024-03-02T00:00:00Z", 30},
		{"week", "2025-01-05T23:59:59Z", "2024-10-14T00:00:00Z", "2024-12-30T00:00:00Z", "2025-01-06T00:00:00Z", 12},
		{"week", "2025-01-06T00:00:00Z", "2024-10-21T00:00:00Z", "2025-01-06T00:00:00Z", "2025-01-13T00:00:00Z", 12},
		{"month", "2024-03-31T23:59:59Z", "2023-04-01T00:00:00Z", "2024-03-01T00:00:00Z", "2024-04-01T00:00:00Z", 12},
	} {
		b, err := buildBucketBounds(tc.interval, historyTime(t, tc.now))
		if err != nil || len(b) != tc.count+1 {
			t.Fatalf("%s buckets for %s: %d, %v", tc.interval, tc.now, len(b), err)
		}
		if b[0].Format(time.RFC3339) != tc.first || b[len(b)-2].Format(time.RFC3339) != tc.last || b[len(b)-1].Format(time.RFC3339) != tc.end {
			t.Fatalf("%s boundaries for %s: %v", tc.interval, tc.now, b)
		}
	}
}

func TestBuildHistoryUpgradeAndPersistence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	legacy, err := sql.Open("sqlite", filepath.Join(dir, "inventory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE clients (ip TEXT PRIMARY KEY, first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL);
		INSERT INTO clients VALUES('::1',0,0);
CREATE TABLE build_runs(started_ns INTEGER PRIMARY KEY, finished_ns INTEGER, outcome TEXT);
INSERT INTO build_runs VALUES (1709251200000000000,1709251200000000001,'succeeded')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := openInventory(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := inventory{db}
	now := historyTime(t, "2024-03-01T00:15:00Z")
	empty, err := s.buildHistory(ctx, "", now)
	if err != nil || empty.Interval != "day" || len(empty.Buckets) != 30 || empty.Totals != (BuildCounts{}) {
		t.Fatalf("empty history %+v, %v", empty, err)
	}
	if err := db.QueryRow("SELECT ip FROM clients WHERE ip='::1'").Scan(new(string)); err != nil {
		t.Fatalf("upgrade lost existing inventory: %v", err)
	}
	if _, err := s.buildHistory(ctx, "second", now); !errors.Is(err, ErrBuildInterval) {
		t.Fatalf("unsupported interval: %v", err)
	}

	midnight := historyTime(t, "2024-03-01T00:00:00Z")
	events := []BuildRecord{
		{Package: "curl-1", Started: midnight.Add(-3 * time.Hour), Finished: midnight.Add(-time.Nanosecond), Outcome: "succeeded"},
		{Package: "git-1", Started: midnight.Add(-3 * time.Hour), Finished: midnight, Outcome: "failed"},
		{Package: "pkg-1", Started: midnight.Add(-3 * time.Hour), Finished: now, Outcome: "succeeded"},
		{Package: "later-1", Started: now.Add(-time.Minute), Finished: now.Add(time.Nanosecond), Outcome: "succeeded"},
	}
	for _, event := range append(events, events[0]) {
		data, err := json.Marshal([]BuildRecord{event})
		if err != nil {
			t.Fatal(err)
		}
		if err := importBuildRecord(ctx, dir, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	for _, interval := range []string{"hour", "day", "month"} {
		history, err := s.buildHistory(ctx, interval, now)
		if err != nil {
			t.Fatal(err)
		}
		end := len(history.Buckets) - 1
		if history.Buckets[end].BuildCounts != (BuildCounts{Succeeded: 1, Failed: 1}) ||
			history.Buckets[end-1].BuildCounts != (BuildCounts{Succeeded: 1}) ||
			history.Totals != (BuildCounts{Succeeded: 2, Failed: 1}) {
			t.Fatalf("%s aggregation: %+v", interval, history)
		}
	}
	h, err := s.buildHistory(ctx, "week", now)
	if err != nil || h.Totals != h.Buckets[len(h.Buckets)-1].BuildCounts {
		t.Fatalf("week aggregation: %+v, %v", h, err)
	}
}

func TestBuildHistoryRetentionAndReplay(t *testing.T) {
	ctx := context.Background()
	db, err := openInventory(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := inventory{db}
	record := func(at string) BuildRecord {
		finished := historyTime(t, at)
		return BuildRecord{Package: "curl-1", Started: finished.Add(-time.Second), Finished: finished, Outcome: "succeeded"}
	}
	old := record("2023-12-31T23:59:59.999999999Z")
	first := record("2024-01-01T00:00:00Z")
	latest := record("2024-12-31T00:00:00Z")
	for _, r := range []BuildRecord{old, first, latest, old, latest} {
		if err := s.recordBuilds(ctx, []BuildRecord{r}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM package_builds").Scan(&count); err != nil || count != 2 {
		t.Fatalf("expired or duplicated runs retained: %d %v", count, err)
	}
	result, err := s.buildHistory(ctx, "month", latest.Finished)
	if err != nil || result.Buckets[0].Succeeded != 1 || result.Buckets[11].Succeeded != 1 || result.Totals.Succeeded != 2 {
		t.Fatalf("retention boundary: %+v %v", result, err)
	}
	noBuild := latest
	noBuild.Outcome = "idle"
	if err := s.recordBuilds(ctx, []BuildRecord{noBuild}); err == nil {
		t.Fatal("idle status accepted as a build")
	}
}

func TestRecentPackageBuilds(t *testing.T) {
	db, err := openInventory(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := inventory{db}
	now := historyTime(t, "2025-02-15T08:20:00Z")
	var batch []BuildRecord
	for i := range 25 {
		finished := now.Add(-time.Duration(i) * time.Minute)
		batch = append(batch, BuildRecord{Package: fmt.Sprintf("pkg-%02d", i), Started: finished.Add(-time.Hour), Finished: finished, Outcome: "succeeded"})
	}
	batch[0].Outcome = "failed"
	// A record just beyond the selected period and one just beyond the as-of time.
	old := batch[0]
	old.Package = "old-1"
	old.Started = now.Add(-25 * time.Hour)
	old.Finished = now.Add(-24 * time.Hour)
	future := batch[0]
	future.Package = "future-1"
	future.Finished = now.Add(time.Second)
	batch = append(batch, old, future, batch[0])
	if err := s.recordBuilds(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	h, err := s.buildHistory(t.Context(), "hour", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Recent) != 20 || h.Totals != (BuildCounts{Succeeded: 24, Failed: 1}) {
		t.Fatalf("unexpected recent package results: %+v", h)
	}
	for i, r := range h.Recent {
		if r != batch[i] {
			t.Fatalf("result %d = %+v, want %+v", i, r, batch[i])
		}
	}
	// All these results are outside the next day's hourly chart, yet remain
	// available when the operator switches to the 30-day view at the same time.
	later := now.Add(25 * time.Hour)
	hourly, err := s.buildHistory(t.Context(), "hour", later)
	if err != nil || len(hourly.Recent) != 0 {
		t.Fatalf("hourly range: %+v, %v", hourly.Recent, err)
	}
	daily, err := s.buildHistory(t.Context(), "day", later)
	if err != nil || len(daily.Recent) != 20 || daily.Recent[0] != future {
		t.Fatalf("daily range: %+v, %v", daily.Recent, err)
	}
}
