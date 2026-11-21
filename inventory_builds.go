package poudrine

import (
	"cmp"
	"context"
	"strings"
	"time"
)

// Poudriere stores the full pkgname, including the optional revision and epoch.
// FreeBSD package names may contain hyphens; their versions cannot.
func splitBuildPackage(pkg string) (name, version string) {
	i := strings.LastIndexByte(pkg, '-')
	if i < 1 || i == len(pkg)-1 || !validIssuePackage(pkg) {
		return "", ""
	}
	return pkg[:i], pkg[i+1:]
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

func (s inventory) recentInventoryBuilds(ctx context.Context) ([]BuildRecord, error) {
	// The timing table already has one record per version, origin and ABI.
	// Join it to retained successes so expired history does not reappear.
	// Only the latest success per package version can supply an additional
	// legacy record when its run has no metadata. Timings belong to that run.
	rows, err := s.db.QueryContext(ctx, `WITH names AS (
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
		name, _ := splitBuildPackage(build.Package)
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
