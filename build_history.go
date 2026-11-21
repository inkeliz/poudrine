package poudrine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const buildHistorySchema = `
CREATE TABLE IF NOT EXISTS package_builds (
 started_ns INTEGER NOT NULL,
 package TEXT NOT NULL,
 outcome TEXT NOT NULL CHECK(outcome IN ('succeeded','failed')),
 finished_ns INTEGER NOT NULL,
 PRIMARY KEY(started_ns,package,outcome)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS package_builds_finished ON package_builds(finished_ns,outcome);
CREATE INDEX IF NOT EXISTS package_builds_latest_success ON package_builds(package,finished_ns DESC,started_ns DESC) WHERE outcome='succeeded';
CREATE TABLE IF NOT EXISTS package_build_times (
 started_ns INTEGER NOT NULL, package TEXT NOT NULL, outcome TEXT NOT NULL,
 seconds INTEGER NOT NULL CHECK(seconds >= 0),
 PRIMARY KEY(started_ns,package,outcome),
 FOREIGN KEY(started_ns,package,outcome) REFERENCES package_builds ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS compiled_package_times (
 package TEXT NOT NULL, origin TEXT NOT NULL, abi TEXT NOT NULL,
 started_ns INTEGER NOT NULL, seconds INTEGER CHECK(seconds >= 0),
 PRIMARY KEY(package,origin,abi)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS push_build_watch (
 endpoint TEXT PRIMARY KEY, since_ns INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS push_build_queue (
 endpoint TEXT NOT NULL, started_ns INTEGER NOT NULL, package TEXT NOT NULL,
 origin TEXT NOT NULL, abi TEXT NOT NULL, finished_ns INTEGER NOT NULL,
 next_try INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(endpoint,started_ns,package)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS push_build_age ON push_build_queue(finished_ns);
CREATE INDEX IF NOT EXISTS push_build_due ON push_build_queue(next_try,finished_ns,package);
CREATE TRIGGER IF NOT EXISTS push_build_expire AFTER DELETE ON package_builds WHEN OLD.outcome='succeeded' BEGIN
 DELETE FROM push_build_queue WHERE started_ns=OLD.started_ns AND package=OLD.package;
END;`

// A package can be built again in a later invocation. Re-reading the same
// Poudriere result list must keep each result exactly once.
type BuildRecord struct {
	Started      time.Time `json:"started"`
	Package      string    `json:"package"`
	Finished     time.Time `json:"finished"`
	Outcome      string    `json:"outcome"`
	Origin       string    `json:"origin,omitempty"`
	ABI          string    `json:"abi,omitempty"`
	BuildSeconds *int64    `json:"build_seconds,omitempty"`
}

const historyBatchSize = 512

func (s inventory) recordBuilds(ctx context.Context, batch []BuildRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insert, err := tx.PrepareContext(ctx, `INSERT INTO package_builds(started_ns,package,outcome,finished_ns)
 VALUES (?,?,?,?) ON CONFLICT(started_ns,package,outcome) DO NOTHING`)
	if err != nil {
		return err
	}
	defer insert.Close()
	timing, err := tx.PrepareContext(ctx, `INSERT INTO package_build_times(started_ns,package,outcome,seconds)
 VALUES (?,?,?,?) ON CONFLICT(started_ns,package,outcome) DO NOTHING`)
	if err != nil {
		return err
	}
	defer timing.Close()
	// A published version can outlive both its build history and its logs.
	// Keep the latest successful run for each version, origin and ABI. The
	// bulk start identifies the run even when log files are replayed in a
	// different order or acquire a different mtime during restoration.
	compiled, err := tx.PrepareContext(ctx, `INSERT INTO compiled_package_times(package,origin,abi,started_ns,seconds)
 VALUES (?,?,?,?,?) ON CONFLICT(package,origin,abi) DO UPDATE SET started_ns=excluded.started_ns, seconds=excluded.seconds
 WHERE excluded.started_ns > started_ns OR (excluded.started_ns = started_ns AND seconds IS NULL AND excluded.seconds IS NOT NULL)`)
	if err != nil {
		return err
	}
	defer compiled.Close()
	for _, r := range batch {
		startNS, finishNS := r.Started.UnixNano(), r.Finished.UnixNano()
		if !time.Unix(0, startNS).Equal(r.Started) || !time.Unix(0, finishNS).Equal(r.Finished) ||
			r.Finished.Before(r.Started) || len(r.Package) > 255 || !jobPackage.MatchString(r.Package) ||
			len(r.Origin) > 255 || (r.Origin != "" && !buildOrigin.MatchString(r.Origin)) || len(r.ABI) > 128 ||
			(r.Outcome != "succeeded" && r.Outcome != "failed") || (r.BuildSeconds != nil && *r.BuildSeconds < 0) {
			return errors.New("invalid package build result")
		}
		result, err := insert.ExecContext(ctx, startNS, r.Package, r.Outcome, finishNS)
		if err != nil {
			return err
		}
		added, err := result.RowsAffected()
		if err != nil {
			return err
		}
		// Replaying retained Poudriere logs fills timings for builds imported
		// by older installations without enqueuing their notifications again.
		if r.BuildSeconds != nil {
			if _, err := timing.ExecContext(ctx, startNS, r.Package, r.Outcome, *r.BuildSeconds); err != nil {
				return err
			}
		}
		if r.Outcome != "succeeded" {
			continue
		}
		abi := packageABI(r.ABI, "")
		if _, err := compiled.ExecContext(ctx, r.Package, r.Origin, abi, startNS, r.BuildSeconds); err != nil {
			return err
		}
		if added != 0 {
			// Enqueue in the history transaction. The eligibility date belongs to
			// the subscription, so late imports and retained logs cannot send old alerts.
			_, err = tx.ExecContext(ctx, `INSERT INTO push_build_queue(endpoint,started_ns,package,origin,abi,finished_ns)
              SELECT endpoint,?,?,?,?,? FROM push_build_watch
              WHERE since_ns <= ?`,
				startNS, r.Package, r.Origin, abi, finishNS, finishNS)
			if err != nil {
				return err
			}
		}
	}
	// Retain twelve calendar months even if an older log is replayed on restart.
	_, err = tx.ExecContext(ctx, `DELETE FROM package_builds WHERE finished_ns <
 1000000000 * unixepoch((SELECT MAX(finished_ns) FROM package_builds) / 1000000000,
 'unixepoch', 'start of month', '-11 months')`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// The supervisor sends fixed-size batches to this unprivileged SQLite writer.
func importBuildRecord(ctx context.Context, dir string, input io.Reader) error {
	var records []BuildRecord
	if err := json.NewDecoder(io.LimitReader(input, 1<<20)).Decode(&records); err != nil {
		return err
	}
	if len(records) == 0 || len(records) > historyBatchSize {
		return errors.New("invalid package build batch size")
	}
	db, err := openInventory(dir, false)
	if err != nil {
		return err
	}
	defer db.Close()
	return (inventory{db}).recordBuilds(ctx, records)
}

type BuildCounts struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

type BuildBucket struct {
	Start time.Time `json:"start"`
	BuildCounts
}

type BuildHistory struct {
	Unit     string        `json:"unit"`
	Interval string        `json:"interval"`
	Buckets  []BuildBucket `json:"buckets"`
	Totals   BuildCounts   `json:"totals"`
	Recent   []BuildRecord `json:"recent"`
}

var ErrBuildInterval = errors.New("interval must be hour, day, week, or month")

// Include the current partial period. UTC keeps days consistent across DST,
// and calculating from the first of each month avoids end-of-month rollover.
func buildBucketBounds(interval string, at time.Time) ([]time.Time, error) {
	now := at.UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var current time.Time
	var count, months, days, hours int
	switch interval {
	case "hour":
		current, count, hours = now.Truncate(time.Hour), 24, 1
	case "day":
		current, count, days = midnight, 30, 1
	case "week":
		weekday := (int(midnight.Weekday()) + 6) % 7
		current, count, days = midnight.AddDate(0, 0, -weekday), 12, 7
	case "month":
		current, count, months = midnight.AddDate(0, 0, 1-now.Day()), 12, 1
	default:
		return nil, ErrBuildInterval
	}
	bounds := make([]time.Time, count+1)
	for i := range bounds {
		offset := i - (count - 1)
		bounds[i] = current.AddDate(0, months*offset, days*offset).Add(time.Duration(hours*offset) * time.Hour)
	}
	return bounds, nil
}

func (s inventory) buildHistory(ctx context.Context, interval string, now time.Time) (BuildHistory, error) {
	if interval == "" {
		interval = "day"
	}
	bounds, err := buildBucketBounds(interval, now)
	if err != nil {
		return BuildHistory{}, err
	}
	// One indexed range lookup per bucket in a single database snapshot.
	// The LEFT JOIN includes empty buckets, also on a brand-new installation.
	slots := make([]string, len(bounds)-1)
	args := make([]any, 0, 2*len(slots)+1)
	for i := range slots {
		slots[i] = "(?,?)"
		args = append(args, bounds[i].UnixNano(), bounds[i+1].UnixNano())
	}
	args = append(args, now.UnixNano())
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return BuildHistory{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `WITH periods(start_ns,end_ns) AS (VALUES `+strings.Join(slots, ",")+`)
		SELECT start_ns,
			count(*) FILTER (WHERE outcome='succeeded'),
			count(*) FILTER (WHERE outcome='failed')
		FROM periods LEFT JOIN package_builds
			ON finished_ns >= start_ns AND finished_ns < end_ns AND finished_ns <= ?
		GROUP BY start_ns ORDER BY start_ns`, args...)
	if err != nil {
		return BuildHistory{}, err
	}
	defer rows.Close()
	out := BuildHistory{Unit: "packages", Interval: interval, Buckets: make([]BuildBucket, 0, len(slots)), Recent: []BuildRecord{}}
	for rows.Next() {
		var bucket BuildBucket
		var startNS int64
		if err := rows.Scan(&startNS, &bucket.Succeeded, &bucket.Failed); err != nil {
			return BuildHistory{}, err
		}
		bucket.Start = time.Unix(0, startNS).UTC()
		out.Buckets = append(out.Buckets, bucket)
		out.Totals.Succeeded += bucket.Succeeded
		out.Totals.Failed += bucket.Failed
	}
	if err := rows.Err(); err != nil {
		return BuildHistory{}, err
	}
	recent, err := tx.QueryContext(ctx, `SELECT package,outcome,started_ns,finished_ns,seconds FROM package_builds
 LEFT JOIN package_build_times USING(started_ns,package,outcome)
 WHERE finished_ns >= ? AND finished_ns <= ?
 ORDER BY finished_ns DESC, package, outcome, started_ns DESC LIMIT 20`, bounds[0].UnixNano(), now.UnixNano())
	if err != nil {
		return BuildHistory{}, err
	}
	defer recent.Close()
	for recent.Next() {
		var r BuildRecord
		var startNS, finishNS int64
		if err := recent.Scan(&r.Package, &r.Outcome, &startNS, &finishNS, &r.BuildSeconds); err != nil {
			return BuildHistory{}, err
		}
		r.Started = time.Unix(0, startNS).UTC()
		r.Finished = time.Unix(0, finishNS).UTC()
		out.Recent = append(out.Recent, r)
	}
	if err := recent.Err(); err != nil {
		return BuildHistory{}, err
	}
	return out, tx.Commit()
}
