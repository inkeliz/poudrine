package database

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/inkeliz/poudrine/internal/packageinfo"
)

// BuildHistorySchema is shared by inventory initialization and standalone administration.
const BuildHistorySchema = packageBuildsSchema + compiledPackageTimesSchema + pushBuildsSchema

const packageBuildsSchema = `
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
`

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

const HistoryBatchSize = 512

func (s DB) RecordBuilds(ctx context.Context, batch []BuildRecord) error {
	tx, err := s.DB.BeginTx(ctx, nil)
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
	// compiled_package_times exposes the latest run for inventory, even if its
	// duration is unknown. Keep one displaced positive timing per target for
	// estimates. Either the incoming log or the existing row may be older.
	prior, err := tx.PrepareContext(ctx, `WITH displaced AS (
 SELECT min(started_ns,?5) AS started, CASE WHEN started_ns < ?5 THEN seconds ELSE ?6 END AS measured
 FROM compiled_package_times WHERE package=?4 AND origin=?2 AND abi=?3 AND (started_ns!=?5 OR seconds=0)
)
INSERT INTO previous_package_build_times(name,origin,abi,started_ns,seconds)
 SELECT ?1,?2,?3,started,measured FROM displaced WHERE measured > 0
 ON CONFLICT(name,origin,abi) DO UPDATE SET started_ns=excluded.started_ns,seconds=excluded.seconds
 WHERE excluded.started_ns > started_ns`)
	if err != nil {
		return err
	}
	defer prior.Close()
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
			r.Finished.Before(r.Started) || len(r.Package) > 255 || !packageinfo.JobPackage.MatchString(r.Package) ||
			len(r.Origin) > 255 || (r.Origin != "" && !packageinfo.BuildOrigin.MatchString(r.Origin)) || len(r.ABI) > 128 ||
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
		abi := packageinfo.PackageABI(r.ABI, "")
		if name, _ := packageinfo.SplitBuildPackage(r.Package); name != "" && r.Origin != "" && abi != "" {
			if _, err := prior.ExecContext(ctx, name, r.Origin, abi, r.Package, startNS, r.BuildSeconds); err != nil {
				return err
			}
		}
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
	AsOf     time.Time     `json:"as_of"`
	Buckets  []BuildBucket `json:"buckets"`
	Totals   BuildCounts   `json:"totals"`
	Recent   []BuildRecord `json:"recent"`
}

var ErrBuildInterval = errors.New("interval must be hour, day, week, or month")
var ErrBuildPeriod = errors.New("expected a chart period start, as_of timestamp and optional cursor within that period")

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

func (s DB) BuildHistory(ctx context.Context, interval string, now time.Time) (BuildHistory, error) {
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
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
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
	out := BuildHistory{Unit: "packages", Interval: interval, AsOf: now.UTC(), Buckets: make([]BuildBucket, 0, len(slots)), Recent: []BuildRecord{}}
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
	if err := rows.Close(); err != nil {
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
	if err := recent.Close(); err != nil {
		return BuildHistory{}, err
	}
	return out, tx.Commit()
}

type BuildPeriodQuery struct {
	Interval string       `json:"interval"`
	Start    time.Time    `json:"start"`
	AsOf     time.Time    `json:"as_of"`
	After    *BuildRecord `json:"after,omitempty"`
}

type BuildPeriodPage struct {
	Packages []BuildRecord `json:"packages"`
	Next     *BuildRecord  `json:"next,omitempty"`
}

const historyPageSize = 50

func (s DB) BuildHistoryPackages(ctx context.Context, q BuildPeriodQuery) (BuildPeriodPage, error) {
	bounds, err := buildBucketBounds(q.Interval, q.AsOf)
	if err != nil {
		return BuildPeriodPage{}, err
	}
	i := slices.IndexFunc(bounds[:len(bounds)-1], q.Start.Equal)
	if i < 0 || !time.Unix(0, q.Start.UnixNano()).Equal(q.Start) ||
		!time.Unix(0, q.AsOf.UnixNano()).Equal(q.AsOf) || q.AsOf.After(time.Now()) {
		return BuildPeriodPage{}, ErrBuildPeriod
	}
	end := bounds[i+1]
	// The empty outcome sorts before both real outcomes, including attempts
	// finishing exactly at the start of the period.
	cursor := BuildRecord{Started: q.Start, Finished: q.Start}
	if q.After != nil {
		cursor = *q.After
		if cursor.Finished.Before(q.Start) || !cursor.Finished.Before(end) || cursor.Finished.After(q.AsOf) ||
			cursor.Started.After(cursor.Finished) || !time.Unix(0, cursor.Started.UnixNano()).Equal(cursor.Started) ||
			(cursor.Outcome != "failed" && cursor.Outcome != "succeeded") ||
			len(cursor.Package) > 255 || !packageinfo.JobPackage.MatchString(cursor.Package) {
			return BuildPeriodPage{}, ErrBuildPeriod
		}
	}
	// The finished index on this WITHOUT ROWID table also contains the
	// primary key. All four columns distinguish attempts sharing a timestamp.
	rows, err := s.QueryContext(ctx, `SELECT finished_ns,outcome,started_ns,package FROM package_builds
 WHERE (finished_ns,outcome,started_ns,package) > (?,?,?,?) AND finished_ns < ? AND finished_ns <= ?
 ORDER BY finished_ns,outcome,started_ns,package LIMIT ?`,
		cursor.Finished.UnixNano(), cursor.Outcome, cursor.Started.UnixNano(), cursor.Package,
		end.UnixNano(), q.AsOf.UnixNano(), historyPageSize+1)
	if err != nil {
		return BuildPeriodPage{}, err
	}
	defer rows.Close()
	page := BuildPeriodPage{Packages: []BuildRecord{}}
	for rows.Next() {
		if len(page.Packages) == historyPageSize {
			page.Next = &page.Packages[historyPageSize-1]
			break
		}
		var r BuildRecord
		var finished, started int64
		if err := rows.Scan(&finished, &r.Outcome, &started, &r.Package); err != nil {
			return BuildPeriodPage{}, err
		}
		r.Started, r.Finished = time.Unix(0, started).UTC(), time.Unix(0, finished).UTC()
		page.Packages = append(page.Packages, r)
	}
	return page, rows.Err()
}

type inventoryBuildTarget struct{ origin, abi string }

func keepLatestBuild(targets map[inventoryBuildTarget]BuildRecord, target inventoryBuildTarget, build BuildRecord) {
	prev, ok := targets[target]
	// Use package ID as a stable tiebreaker if two finish and start times match.
	order := cmp.Or(build.Finished.Compare(prev.Finished), build.Started.Compare(prev.Started), strings.Compare(build.Package, prev.Package))
	if !ok || order > 0 || order == 0 && prev.BuildSeconds == nil && build.BuildSeconds != nil {
		targets[target] = build
	}
}

// Runs imported before origin/ABI recording, or replayed with more complete
// metadata, can describe the same target. Prefer the known identity if it is
// unambiguous. Preserve distinct known targets when a jail or port changes.
func resolveBuildTarget(target inventoryBuildTarget, build BuildRecord, targets map[inventoryBuildTarget]BuildRecord) (inventoryBuildTarget, bool) {
	if len(targets) == 1 || target.origin != "" && target.abi != "" {
		return target, true
	}
	origin, abi := target.origin, target.abi
	var multipleOrigins, multipleABIs, newerKnown bool
	for other, record := range targets {
		if other == target || target.origin != "" && other.origin != "" && other.origin != target.origin ||
			target.abi != "" && other.abi != "" && other.abi != target.abi {
			continue
		}
		if other.origin != "" {
			if origin != "" && origin != other.origin {
				multipleOrigins = true
			} else {
				origin = other.origin
			}
		}
		if other.abi != "" {
			if abi != "" && abi != other.abi {
				multipleABIs = true
			} else {
				abi = other.abi
			}
		}
		if (other.origin != "" && target.origin == "" || other.abi != "" && target.abi == "") && !record.Finished.Before(build.Finished) {
			newerKnown = true
		}
	}
	if multipleOrigins {
		origin = target.origin
	}
	if multipleABIs {
		abi = target.abi
	}
	return inventoryBuildTarget{origin, abi}, !(newerKnown && (multipleOrigins || multipleABIs))
}

func (s DB) RecentInventoryBuilds(ctx context.Context) ([]BuildRecord, error) {
	// The timing table already has one record per version, origin and ABI.
	// Join it to retained successes so expired history does not reappear.
	// Only the latest success per package version can supply an additional
	// legacy record when its run has no metadata. Timings belong to that run.
	rows, err := s.DB.QueryContext(ctx, `WITH names AS (
 SELECT DISTINCT package FROM package_builds WHERE outcome='succeeded'
), candidates AS (
 SELECT b.package, b.started_ns, b.finished_ns, c.origin, c.abi, c.seconds
 FROM compiled_package_times c JOIN package_builds b
   ON b.package=c.package AND b.started_ns=c.started_ns AND b.outcome='succeeded'
 UNION ALL
 SELECT r.package, r.started_ns, r.finished_ns, '', '', NULL
 FROM names n JOIN package_builds r ON r.package=n.package AND r.outcome='succeeded' AND r.started_ns=(
   SELECT b.started_ns FROM package_builds b WHERE b.package=n.package AND b.outcome='succeeded'
   ORDER BY b.finished_ns DESC, b.started_ns DESC LIMIT 1)
 WHERE NOT EXISTS
   (SELECT 1 FROM compiled_package_times c WHERE c.package=r.package AND c.started_ns=r.started_ns)
)
SELECT c.package, c.started_ns, c.finished_ns, c.origin, c.abi, coalesce(t.seconds,c.seconds)
FROM candidates c LEFT JOIN package_build_times t
  ON t.package=c.package AND t.started_ns=c.started_ns AND t.outcome='succeeded'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byName := make(map[string]map[inventoryBuildTarget]BuildRecord)
	for rows.Next() {
		build := BuildRecord{Outcome: "succeeded"}
		var started, finished int64
		if err := rows.Scan(&build.Package, &started, &finished, &build.Origin, &build.ABI, &build.BuildSeconds); err != nil {
			return nil, err
		}
		name, _ := packageinfo.SplitBuildPackage(build.Package)
		if name == "" {
			continue
		}
		build.Started, build.Finished = time.Unix(0, started).UTC(), time.Unix(0, finished).UTC()
		if byName[name] == nil {
			byName[name] = make(map[inventoryBuildTarget]BuildRecord)
		}
		keepLatestBuild(byName[name], inventoryBuildTarget{build.Origin, build.ABI}, build)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var builds []BuildRecord
	for _, targets := range byName {
		resolved := make(map[inventoryBuildTarget]BuildRecord, len(targets))
		for target, build := range targets {
			key, keep := resolveBuildTarget(target, build, targets)
			if keep {
				build.Origin, build.ABI = key.origin, key.abi
				keepLatestBuild(resolved, key, build)
			}
		}
		for _, build := range resolved {
			builds = append(builds, build)
		}
	}
	return builds, nil
}
