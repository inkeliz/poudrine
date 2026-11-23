package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"time"

	"github.com/inkeliz/poudrine/internal/packageinfo"
)

type compiledPackageTime struct {
	Package, Origin, ABI string
	Seconds              *int64
}

func (s DB) CompiledPackageTimes(ctx context.Context, names []string) ([]compiledPackageTime, error) {
	keys, err := json.Marshal(names)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT package,origin,abi,seconds FROM compiled_package_times
 WHERE package IN (SELECT value FROM json_each(?))
 ORDER BY package, started_ns DESC, seconds IS NOT NULL DESC`, string(keys))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []compiledPackageTime
	for rows.Next() {
		var t compiledPackageTime
		if err := rows.Scan(&t.Package, &t.Origin, &t.ABI, &t.Seconds); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

type BuildTimeTarget struct {
	Package, Origin, ABI string
}

// LastBuildDurations returns recorded seconds for the latest timed successful
// build of each target, including previous versions of the same package.
// A bulk start cutoff prevents an already completed job in the current run
// from becoming its own baseline during a status refresh.
func (s DB) LastBuildDurations(ctx context.Context, before time.Time, targets []BuildTimeTarget) (map[BuildTimeTarget]int64, error) {
	type lookup struct{ Name, Origin, ABI string }
	keys := make(map[int]lookup)
	for i, target := range targets {
		name, _ := packageinfo.SplitBuildPackage(target.Package)
		if name != "" && target.Origin != "" && target.ABI != "" {
			keys[i] = lookup{name, target.Origin, packageinfo.PackageABI(target.ABI, "")}
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	request, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	cutoff := int64(math.MaxInt64)
	if !before.IsZero() {
		cutoff = before.UnixNano()
	}
	// Package names have already been checked and contain no glob wildcards.
	// Versions have no hyphens, so pkg-extra-1 must not match pkg-2. Stop
	// the indexed version lookup at its first match before comparing the
	// newest displaced timing.
	rows, err := s.QueryContext(ctx, `SELECT j.key, (
 SELECT seconds FROM (
  SELECT * FROM (
   SELECT started_ns,seconds FROM compiled_package_times
   WHERE origin=j.value->>'Origin' AND abi=j.value->>'ABI' AND started_ns < ?1 AND seconds > 0
     AND package GLOB (j.value->>'Name')||'-*' AND package NOT GLOB (j.value->>'Name')||'-*-*'
   ORDER BY started_ns DESC,package LIMIT 1
  )
  UNION ALL
  SELECT started_ns,seconds FROM previous_package_build_times
  WHERE name=j.value->>'Name' AND origin=j.value->>'Origin' AND abi=j.value->>'ABI' AND started_ns < ?1
 ) ORDER BY started_ns DESC LIMIT 1
) FROM json_each(?2) j`, cutoff, string(request))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[BuildTimeTarget]int64)
	for rows.Next() {
		var i int
		var seconds sql.NullInt64
		if err := rows.Scan(&i, &seconds); err != nil {
			return nil, err
		}
		if seconds.Valid {
			result[targets[i]] = seconds.Int64
		}
	}
	return result, rows.Err()
}

const compiledPackageTimesSchema = `CREATE TABLE IF NOT EXISTS compiled_package_times (
 package TEXT NOT NULL, origin TEXT NOT NULL, abi TEXT NOT NULL,
 started_ns INTEGER NOT NULL, seconds INTEGER CHECK(seconds >= 0),
 PRIMARY KEY(package,origin,abi)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS compiled_package_duration_lookup ON compiled_package_times(origin,abi,started_ns DESC,package) WHERE seconds > 0;
CREATE TABLE IF NOT EXISTS previous_package_build_times (
 name TEXT NOT NULL, origin TEXT NOT NULL, abi TEXT NOT NULL,
 started_ns INTEGER NOT NULL, seconds INTEGER NOT NULL CHECK(seconds > 0),
 PRIMARY KEY(name,origin,abi)
) WITHOUT ROWID;
`
