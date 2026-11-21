package poudrine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const poudriereTimestamp = "2006-01-02T15:04:05-0700"

type inheritedBuild struct {
	origin string
	stamp  time.Time
}

func archiveBuildTime(raw []byte) time.Time {
	var m struct {
		Annotations struct {
			Stamp string `json:"build_timestamp"`
		}
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return time.Time{}
	}
	stamp, _ := time.Parse(poudriereTimestamp, m.Annotations.Stamp)
	return stamp
}

// Stash/reuse can carry an uncommitted package into the next bulk without
// writing another success for it. Match the actual staged archive to a native
// job start and a completed result in that specific run. Archives without a
// build timestamp (e.g. reproducible builds) await native publication.
func (s *liveArchiveStore) inheritedBuilds(ctx context.Context, master, all *os.File, current map[string]livePackage) (map[string]inheritedBuild, error) {
	found := make(map[string]inheritedBuild)
	sources := make(map[string]os.FileInfo)
	pending := make(map[string]time.Time)
	entries, err := all.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, ok := strings.CutSuffix(entry.Name(), ".pkg")
		name, version := splitBuildPackage(id)
		if !ok || name == "" {
			continue
		}
		if p := current[name]; p.file != nil && comparePkgVersions(p.manifest.pkg.Version, version) >= 0 {
			continue
		}
		info, build := s.inspectInherited(all, name, version, id)
		if info != nil {
			sources[id] = info
		}
		if build.origin != "" {
			found[id] = build
		} else if !build.stamp.IsZero() && build.stamp.Before(s.started.Truncate(time.Second)) {
			pending[id] = build.stamp
		}
	}
	if len(pending) == 0 {
		s.inheritedSources = sources
		return found, nil
	}
	// Only search past runs for new or replaced files. A large bulk may add
	// archives on every poll while most of its retained files stay unchanged.
	entries, err = master.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var runs []time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		t, err := time.Parse(buildNameFormat, entry.Name())
		if err == nil && t.Format(buildNameFormat) == entry.Name() && t.Before(s.started) {
			runs = append(runs, t)
		}
	}
	slices.SortFunc(runs, time.Time.Compare)
	wanted := make(map[time.Time]map[string]time.Time)
	for id, stamp := range pending {
		i := sort.Search(len(runs), func(i int) bool { return runs[i].Truncate(time.Second).After(stamp) }) - 1
		if i < 0 {
			continue
		}
		selected := runs[i]
		if wanted[selected] == nil {
			wanted[selected] = make(map[string]time.Time)
		}
		wanted[selected][id] = stamp
	}
	for started, ids := range wanted {
		run, err := openLogPath(master, started.Format(buildNameFormat), true)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		err = scanBuildResults(ctx, run, "built", func(_ string, fields []string) bool {
			id := fields[1]
			if stamp, ok := ids[id]; ok && matchesNativeJobStart(run, id, stamp) {
				found[id] = inheritedBuild{origin: fields[0], stamp: stamp}
			}
			return true
		})
		run.Close()
		if err != nil {
			return nil, err
		}
	}
	s.inheritedSources = sources
	return found, nil
}

// A nonempty origin means the same archive has already been matched to its
// build log. A timestamp alone still needs corroboration from the older run.
func (s *liveArchiveStore) inspectInherited(all *os.File, name, version, id string) (os.FileInfo, inheritedBuild) {
	source, err := openStageArchive(all, id)
	if err != nil {
		return nil, inheritedBuild{}
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, inheritedBuild{}
	}
	// Poudriere hardlinks the last repository into its stage. The index may
	// still omit a version whose archive is already present in both places.
	committed, err := os.Stat(filepath.Join(s.repoDir, "All", id+".pkg"))
	if err == nil && os.SameFile(info, committed) &&
		(s.offeredVersion == nil || s.versionAlreadyOffered(name, version)) {
		return nil, inheritedBuild{}
	}
	if last := s.inheritedSources[id]; last != nil && sameArchive(last, info) {
		return info, s.inherited[id]
	}
	fetched, err := fetchedByPoudriere(s.repoDir, id+".pkg", info)
	if err != nil {
		return nil, inheritedBuild{}
	}
	if fetched {
		return info, inheritedBuild{}
	}
	raw, err := liveCompactJSON(source)
	if err != nil {
		return nil, inheritedBuild{}
	}
	return info, inheritedBuild{stamp: archiveBuildTime(raw)}
}

func matchesNativeJobStart(run *os.File, id string, stamp time.Time) bool {
	f, err := openLogPath(run, "logs/"+id+".log", false)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	header, err := io.ReadAll(io.NewSectionReader(f, 0, 4096))
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(header), "\n") {
		line = line[len(logTimePrefix.FindString(line)):]
		date, ok := strings.CutPrefix(line, "build started at ")
		if !ok {
			continue
		}
		started, err := time.ParseInLocation(time.UnixDate, strings.TrimSpace(date), time.Local)
		// Poudriere invokes date twice: the UTC package annotation can be
		// one second later than the local timestamp at the top of the log.
		return err == nil && !stamp.Before(started) && stamp.Sub(started) <= time.Second
	}
	return false
}
