package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/testutil"
)

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

	db, err := database.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := *db
	now := testutil.Time(t, "2024-03-01T00:15:00Z")
	empty, err := s.BuildHistory(ctx, "", now)
	if err != nil || empty.Interval != "day" || len(empty.Buckets) != 30 || empty.Totals != (database.BuildCounts{}) {
		t.Fatalf("empty history %+v, %v", empty, err)
	}
	if err := db.QueryRow("SELECT ip FROM clients WHERE ip='::1'").Scan(new(string)); err != nil {
		t.Fatalf("upgrade lost existing inventory: %v", err)
	}
	if _, err := s.BuildHistory(ctx, "second", now); !errors.Is(err, database.ErrBuildInterval) {
		t.Fatalf("unsupported interval: %v", err)
	}

	midnight := testutil.Time(t, "2024-03-01T00:00:00Z")
	events := []database.BuildRecord{
		{Package: "curl-1", Started: midnight.Add(-3 * time.Hour), Finished: midnight.Add(-time.Nanosecond), Outcome: "succeeded"},
		{Package: "git-1", Started: midnight.Add(-3 * time.Hour), Finished: midnight, Outcome: "failed"},
		{Package: "pkg-1", Started: midnight.Add(-3 * time.Hour), Finished: now, Outcome: "succeeded"},
		{Package: "later-1", Started: now.Add(-time.Minute), Finished: now.Add(time.Nanosecond), Outcome: "succeeded"},
	}
	for _, event := range append(events, events[0]) {
		data, err := json.Marshal([]database.BuildRecord{event})
		if err != nil {
			t.Fatal(err)
		}
		if err := importBuildRecord(ctx, dir, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	for _, interval := range []string{"hour", "day", "month"} {
		history, err := s.BuildHistory(ctx, interval, now)
		if err != nil {
			t.Fatal(err)
		}
		end := len(history.Buckets) - 1
		if history.Buckets[end].BuildCounts != (database.BuildCounts{Succeeded: 1, Failed: 1}) ||
			history.Buckets[end-1].BuildCounts != (database.BuildCounts{Succeeded: 1}) ||
			history.Totals != (database.BuildCounts{Succeeded: 2, Failed: 1}) {
			t.Fatalf("%s aggregation: %+v", interval, history)
		}
	}
	h, err := s.BuildHistory(ctx, "week", now)
	if err != nil || h.Totals != h.Buckets[len(h.Buckets)-1].BuildCounts {
		t.Fatalf("week aggregation: %+v, %v", h, err)
	}
}
