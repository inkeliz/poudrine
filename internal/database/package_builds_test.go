package database

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/testutil"
)

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
		b, err := buildBucketBounds(tc.interval, testutil.Time(t, tc.now))
		if err != nil || len(b) != tc.count+1 {
			t.Fatalf("%s buckets for %s: %d, %v", tc.interval, tc.now, len(b), err)
		}
		if b[0].Format(time.RFC3339) != tc.first || b[len(b)-2].Format(time.RFC3339) != tc.last || b[len(b)-1].Format(time.RFC3339) != tc.end {
			t.Fatalf("%s boundaries for %s: %v", tc.interval, tc.now, b)
		}
	}
}

func TestSuccessfulBuildTotals(t *testing.T) {
	db, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	at := testutil.Time(t, "2024-02-29T19:00:00-05:00") // March 1 at midnight UTC.
	var records []BuildRecord
	for _, result := range []struct {
		finish  time.Time
		outcome string
	}{
		{at.AddDate(0, 0, -6).Add(-time.Nanosecond), "succeeded"},
		{at.AddDate(0, 0, -6), "succeeded"},
		{at.AddDate(0, 0, -1), "succeeded"},
		{at.Add(-time.Nanosecond), "succeeded"},
		{at, "failed"},
		{at, "succeeded"},
		{at.Add(time.Nanosecond), "succeeded"},
	} {
		records = append(records, BuildRecord{
			Package: "curl-1", Started: result.finish.Add(-time.Second), Finished: result.finish, Outcome: result.outcome,
		})
	}
	if err := db.RecordBuilds(ctx, records); err != nil {
		t.Fatal(err)
	}
	today, yesterday, week, err := db.SuccessfulBuildTotals(ctx, at)
	if err != nil || today != 1 || yesterday != 2 || week != 4 {
		t.Fatalf("build totals at midnight UTC: today=%d yesterday=%d week=%d err=%v", today, yesterday, week, err)
	}
}

func TestBuildHistoryRetentionAndReplay(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := *db
	record := func(at string) BuildRecord {
		finished := testutil.Time(t, at)
		return BuildRecord{Package: "curl-1", Started: finished.Add(-time.Second), Finished: finished, Outcome: "succeeded"}
	}
	old := record("2023-12-31T23:59:59.999999999Z")
	first := record("2024-01-01T00:00:00Z")
	latest := record("2024-12-31T00:00:00Z")
	for _, r := range []BuildRecord{old, first, latest, old, latest} {
		if err := s.RecordBuilds(ctx, []BuildRecord{r}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM package_builds").Scan(&count); err != nil || count != 2 {
		t.Fatalf("expired or duplicated runs retained: %d %v", count, err)
	}
	result, err := s.BuildHistory(ctx, "month", latest.Finished)
	if err != nil || result.Buckets[0].Succeeded != 1 || result.Buckets[11].Succeeded != 1 || result.Totals.Succeeded != 2 {
		t.Fatalf("retention boundary: %+v %v", result, err)
	}
	noBuild := latest
	noBuild.Outcome = "idle"
	if err := s.RecordBuilds(ctx, []BuildRecord{noBuild}); err == nil {
		t.Fatal("idle status accepted as a build")
	}
}

func TestRecentPackageBuilds(t *testing.T) {
	db, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := *db
	now := testutil.Time(t, "2025-02-15T08:20:00Z")
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
	if err := s.RecordBuilds(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	h, err := s.BuildHistory(t.Context(), "hour", now)
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
	hourly, err := s.BuildHistory(t.Context(), "hour", later)
	if err != nil || len(hourly.Recent) != 0 {
		t.Fatalf("hourly range: %+v, %v", hourly.Recent, err)
	}
	daily, err := s.BuildHistory(t.Context(), "day", later)
	if err != nil || len(daily.Recent) != 20 || daily.Recent[0] != future {
		t.Fatalf("daily range: %+v, %v", daily.Recent, err)
	}
}

func TestBuildHistoryPeriodPages(t *testing.T) {
	db, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Leap day and the current partial period exercise each chart interval.
	asOf := testutil.Time(t, "2024-02-29T21:37:08.123456789Z")
	var batch []BuildRecord
	for i := range historyPageSize + 3 {
		batch = append(batch, BuildRecord{
			Package: fmt.Sprintf("pkg-%03d", i), Outcome: "succeeded",
			Started: asOf.Add(-time.Hour), Finished: asOf,
		})
	}
	retry := batch[0]
	retry.Outcome = "failed"
	batch = append(batch, retry)
	future := batch[0]
	future.Package = "future-1"
	future.Finished = asOf.Add(time.Nanosecond)
	batch = append(batch, future)
	for _, finish := range []string{"2024-01-31T23:59:59Z", "2024-02-01T00:00:00Z", "2024-02-26T00:00:00Z", "2024-02-29T00:00:00Z", "2024-02-29T21:00:00Z"} {
		at := testutil.Time(t, finish)
		batch = append(batch, BuildRecord{Package: "curl-1", Outcome: "succeeded", Started: at.Add(-time.Minute), Finished: at})
	}
	if err := db.RecordBuilds(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	for _, interval := range []string{"hour", "day", "week", "month"} {
		h, err := db.BuildHistory(t.Context(), interval, asOf)
		if err != nil {
			t.Fatal(err)
		}
		for _, bucket := range h.Buckets {
			args := BuildPeriodQuery{Interval: interval, Start: bucket.Start, AsOf: h.AsOf}
			var counts BuildCounts
			seen := map[BuildRecord]bool{}
			for {
				page, err := db.BuildHistoryPackages(t.Context(), args)
				if err != nil {
					t.Fatal(err)
				}
				if page.Packages == nil || len(page.Packages) > historyPageSize {
					t.Fatalf("%s bucket %s: invalid page length %d", interval, bucket.Start, len(page.Packages))
				}
				for _, r := range page.Packages {
					if seen[r] || r.Finished.Before(bucket.Start) || r.Finished.After(asOf) {
						t.Fatalf("%s bucket %s: repeated or out-of-period result %+v", interval, bucket.Start, r)
					}
					seen[r] = true
					if r.Outcome == "failed" {
						counts.Failed++
					} else {
						counts.Succeeded++
					}
				}
				if page.Next == nil {
					break
				}
				if len(page.Packages) != historyPageSize || *page.Next != page.Packages[historyPageSize-1] {
					t.Fatal("unexpected cursor", page.Next)
				}
				args.After = page.Next
			}
			if counts != bucket.BuildCounts {
				t.Fatalf("%s bucket %s has %+v, chart has %+v", interval, bucket.Start, counts, bucket.BuildCounts)
			}
		}
	}
	for _, q := range []BuildPeriodQuery{
		{Interval: "day", AsOf: asOf, Start: asOf},
		{Interval: "day", AsOf: asOf, Start: asOf.AddDate(0, 0, -31).Truncate(24 * time.Hour)},
		{Interval: "day", AsOf: asOf, Start: asOf.Truncate(24 * time.Hour), After: &future},
		{Interval: "day"},
	} {
		if _, err := db.BuildHistoryPackages(t.Context(), q); !errors.Is(err, ErrBuildPeriod) {
			t.Fatalf("invalid bucket or cursor accepted: %+v: %v", q, err)
		}
	}
}
