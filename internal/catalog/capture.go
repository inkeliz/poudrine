package catalog

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"golang.org/x/sys/unix"
)

// The native stage and its marker identify a run even when the worker has
// restarted. A successful log entry alone cannot publish a package.
type LiveArchiveStore struct {
	Dir, RepoDir     string `json:"-"`
	offeredVersion   func(string) string
	offered          map[string]string
	started          time.Time
	stage            os.FileInfo
	inputs           [5]os.FileInfo // All, Hashed, built, fetched, and the fetch cache
	inherited        map[string]inheritedBuild
	inheritedSources map[string]os.FileInfo
	priorFetches     map[string][]string
	retryFetch       time.Time
	sources          map[string]os.FileInfo
	packages         map[string]LivePackage
	failures         map[string]liveCaptureFailure
}

type liveCaptureFailure struct {
	origin  string
	message string
	retry   time.Time
}

func (s *LiveArchiveStore) reset(stage os.FileInfo, started time.Time) {
	*s = LiveArchiveStore{Dir: s.Dir, RepoDir: s.RepoDir, offeredVersion: s.offeredVersion, stage: stage, started: started,
		packages: make(map[string]LivePackage), sources: make(map[string]os.FileInfo), failures: make(map[string]liveCaptureFailure)}
}

func stageBuildName(stage *os.File) (time.Time, error) {
	marker, err := builder.OpenLogPath(stage, ".buildname", false)
	if err != nil {
		return time.Time{}, err
	}
	defer marker.Close()
	info, err := marker.Stat()
	if err != nil {
		return time.Time{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 128 {
		return time.Time{}, errors.New("invalid Poudriere stage marker")
	}
	data, err := io.ReadAll(io.LimitReader(marker, 129))
	if err != nil {
		return time.Time{}, err
	}
	name := strings.TrimSpace(string(data))
	t, err := time.Parse(builder.BuildNameFormat, name)
	if err != nil || name != t.Format(builder.BuildNameFormat) {
		return time.Time{}, errors.New("stage has no Poudrine build name")
	}
	return t, nil
}

func (s *LiveArchiveStore) Scan(ctx context.Context, cfg config.Builder) (map[string]LivePackage, error) {
	path := filepath.Join(s.RepoDir, ".building")
	stage, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		if s.stage != nil {
			s.reset(nil, time.Time{})
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	info, err := stage.Stat()
	if err != nil {
		return nil, err
	}
	started, err := stageBuildName(stage)
	if err != nil {
		return nil, err
	}
	// A fresh .building begins as a hardlinked copy of .latest, with the
	// previous run's marker until Poudriere has finished preparing the queue.
	// Its archives are already served through the committed repository.
	if committed, err := os.OpenFile(filepath.Join(s.RepoDir, ".latest"), os.O_RDONLY|syscall.O_DIRECTORY, 0); err == nil {
		published, readErr := stageBuildName(committed)
		committed.Close()
		if readErr == nil && published.Equal(started) {
			s.reset(nil, time.Time{})
			return nil, nil
		}
	}
	if s.stage == nil || !os.SameFile(s.stage, info) || !s.started.Equal(started) {
		s.reset(info, started)
	}
	logs, err := builder.OpenBulkLogs(cfg)
	if err != nil {
		return nil, err
	}
	defer logs.Close()
	master, err := builder.OpenLogPath(logs, cfg.Jail+"-"+cfg.Ports, true)
	if err != nil {
		return nil, err
	}
	defer master.Close()
	run, err := builder.OpenLogPath(master, started.Format(builder.BuildNameFormat), true)
	if err != nil {
		return nil, err
	}
	defer run.Close()
	all, err := builder.OpenLogPath(stage, "All", true)
	if err != nil {
		return nil, err
	}
	defer all.Close()

	// Completed native archives are renamed into All. A settled stage can
	// stay here for days. Its directory and result-list identities let each
	// poll skip the per-package I/O until another package arrives or goes.
	var inputs [5]os.FileInfo
	inputs[0], err = all.Stat()
	if err != nil {
		return nil, err
	}
	logDir := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports, started.Format(builder.BuildNameFormat))
	for i, path := range []string{filepath.Join(path, "All", "Hashed"), filepath.Join(logDir, ".poudriere.ports.built"), filepath.Join(logDir, ".poudriere.ports.fetched"), filepath.Join(s.RepoDir, ".pkg-cache")} {
		inputs[i+1], err = os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if s.unchanged(inputs) {
		return maps.Clone(s.packages), nil
	}
	// A resumed native stage may be undergoing deletions and replacements.
	// Reconcile the whole set against this pass's successful, verified files.
	next := make(map[string]LivePackage, len(s.packages))
	s.offered = make(map[string]string)
	results := make(map[string]bool)
	visit := func(id, origin string, stamp time.Time) {
		results[id] = true
		name, version := packageinfo.SplitBuildPackage(id)
		if old := next[name]; old.File != nil && ComparePkgVersions(old.Manifest.Pkg.Version, version) >= 0 {
			return
		}
		p, err := s.stagedPackage(ctx, all, id, origin, stamp)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			previous := s.failures[id]
			if !errors.Is(err, os.ErrNotExist) && err.Error() != previous.message {
				log.Printf("stage built package %s: %v", id, err)
			}
			s.failures[id] = liveCaptureFailure{origin: origin, message: err.Error(), retry: time.Now().Add(time.Minute)}
		}
		if p.File != nil {
			next[p.Manifest.Pkg.Name] = p
		}
	}
	if err := builder.ScanBuildResults(ctx, run, "built", func(_ string, fields []string) bool {
		visit(fields[1], fields[0], time.Time{})
		return ctx.Err() == nil
	}); err != nil {
		return nil, err
	}
	inherited, err := s.inheritedBuilds(ctx, master, all, next)
	if err != nil {
		return nil, err
	}
	s.inherited = inherited
	for id, build := range s.inherited {
		visit(id, build.origin, build.stamp)
	}
	for id := range s.failures {
		if !results[id] {
			delete(s.failures, id)
		}
	}
	if err := s.addFetchedDependencies(ctx, master, run, all, next); err != nil {
		if ctx.Err() == nil && !time.Now().Before(s.retryFetch) {
			log.Printf("stage worker-fetched dependencies: %v", err)
		}
		s.retryFetch = time.Now().Add(time.Minute)
	} else {
		s.retryFetch = time.Time{}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	latest, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		s.reset(nil, time.Time{})
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	current, err := stageBuildName(stage)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(latest, info) || !current.Equal(started) {
		return nil, errors.New("Poudriere stage changed during capture")
	}
	s.packages = next
	s.inputs = inputs
	return maps.Clone(next), nil
}

func (s *LiveArchiveStore) unchanged(inputs [5]os.FileInfo) bool {
	for i, current := range inputs {
		previous := s.inputs[i]
		if (current == nil) != (previous == nil) || current != nil && !sameArchive(previous, current) {
			return false
		}
	}
	for _, failure := range s.failures {
		if !time.Now().Before(failure.retry) {
			return false
		}
	}
	if !s.retryFetch.IsZero() && !time.Now().Before(s.retryFetch) {
		return false
	}
	for name, version := range s.offered {
		if s.offeredVersion == nil {
			return false
		}
		if baseline := s.offeredVersion(name); baseline == "" || ComparePkgVersions(baseline, version) < 0 {
			return false
		}
	}
	return true
}

func (s *LiveArchiveStore) versionAlreadyOffered(name, version string) bool {
	if s.offeredVersion == nil {
		return false
	}
	baseline := s.offeredVersion(name)
	if baseline == "" || ComparePkgVersions(baseline, version) < 0 {
		return false
	}
	if old := s.offered[name]; old == "" || ComparePkgVersions(version, old) > 0 {
		s.offered[name] = version
	}
	return true
}

// pkg repo --hash --symlink puts a relative alias at All/<pkg>.pkg.
// Resolve that one native layout descriptor-relatively, rejecting any other
// target or nested symlink. The manifest and complete archive are checked next.
func openStageArchive(all *os.File, id string) (*os.File, error) {
	f, err := builder.OpenLogPath(all, id+".pkg", false)
	// O_NOFOLLOW reports EMLINK on FreeBSD and ELOOP on Linux.
	if !errors.Is(err, syscall.EMLINK) && !errors.Is(err, syscall.ELOOP) {
		return f, err
	}
	var buf [512]byte
	n, err := unix.Readlinkat(int(all.Fd()), id+".pkg", buf[:])
	if err != nil {
		return nil, err
	}
	target := string(buf[:n])
	name, ok := strings.CutPrefix(target, "Hashed/")
	if !ok || !isHashedArchive(name, id) {
		return nil, errors.New("unexpected Poudriere package symlink")
	}
	return builder.OpenLogPath(all, target, false)
}

func isHashedArchive(name, id string) bool {
	suffix, ok := strings.CutPrefix(name, id+"~")
	hash, ext := strings.CutSuffix(suffix, ".pkg")
	return ok && ext && hash != "" && strings.Trim(hash, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ$") == ""
}

func (s *LiveArchiveStore) stagedPackage(ctx context.Context, all *os.File, id, origin string, stamp time.Time) (LivePackage, error) {
	name, version := packageinfo.SplitBuildPackage(id)
	if name == "" || time.Now().Before(s.failures[id].retry) {
		return LivePackage{}, nil
	}
	source, err := openStageArchive(all, id)
	if err != nil {
		return LivePackage{}, err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return LivePackage{}, err
	}
	fetched, err := fetchedByPoudriere(s.RepoDir, id+".pkg", info)
	if err != nil {
		return LivePackage{}, err
	}
	if fetched {
		return LivePackage{}, errors.New("staged archive belongs to Poudriere's download cache")
	}
	if old := s.packages[name]; !old.Fetched && old.Manifest.Pkg.Version == version && old.File != nil && s.sources[id] != nil && sameArchive(s.sources[id], info) {
		return old, nil
	}
	p, err := s.capture(ctx, source, info, id, origin, stamp)
	if err == nil {
		s.sources[id] = info
		delete(s.failures, id)
	}
	return p, err
}

func (s *LiveArchiveStore) capture(ctx context.Context, source *os.File, before os.FileInfo, id, origin string, stamp time.Time) (LivePackage, error) {
	if !before.Mode().IsRegular() || before.Size() <= 0 {
		return LivePackage{}, errors.New("unexpected staged package file")
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return LivePackage{}, err
	}
	tmp, err := os.CreateTemp(s.Dir, ".capture-")
	if err != nil {
		return LivePackage{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	stop := context.AfterFunc(ctx, func() { source.Close() })
	defer stop()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(source, before.Size()))
	if err != nil {
		return LivePackage{}, err
	}
	if err := ctx.Err(); err != nil {
		return LivePackage{}, err
	}
	after, err := source.Stat()
	if err != nil {
		return LivePackage{}, err
	}
	if n != before.Size() || !sameArchive(before, after) {
		return LivePackage{}, errors.New("staged package changed while copying")
	}
	sum := hex.EncodeToString(h.Sum(nil))
	m, err := ReadLiveManifest(tmp, sum)
	if err != nil {
		return LivePackage{}, err
	}
	// The result list can omit the default flavor, including when it names
	// a subpackage. Honor every flavor and subpackage it explicitly names.
	if m.Pkg.Name+"-"+m.Pkg.Version != id || !matchesResultOrigin(m.Pkg, origin) {
		return LivePackage{}, fmt.Errorf("compact manifest does not match successful build %s %s", origin, id)
	}

	if !stamp.IsZero() && !archiveBuildTime(m.Raw).Equal(stamp) {
		return LivePackage{}, errors.New("retained archive differs from recorded build timestamp")
	}
	dest := filepath.Join(s.Dir, sum+".pkg")
	// Link only validated bytes into the addressable namespace. A prior run
	// may already own this path; verify those bytes before reusing the inode.
	if err := os.Link(tmp.Name(), dest); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return LivePackage{}, err
		}
		old, err := os.Lstat(dest)
		if err != nil {
			return LivePackage{}, err
		}
		file := &CompiledArchive{Path: dest, Info: old}
		if old.Mode().IsRegular() && file.verify(m.Pkg) == nil {
			return LivePackage{Manifest: m, File: file}, nil
		}
		if err := os.Rename(tmp.Name(), dest); err != nil {
			return LivePackage{}, err
		}
	}
	info, err := tmp.Stat()
	if err != nil {
		return LivePackage{}, err
	}
	return LivePackage{Manifest: m, File: &CompiledArchive{Path: dest, Info: info, verified: true}}, nil
}

func matchesResultOrigin(p packageinfo.Package, origin string) bool {
	unflavored := p.Origin
	if _, sub, ok := strings.Cut(p.BuildOrigin, "~"); ok {
		unflavored += "~" + sub
	}
	return origin == p.BuildOrigin || origin == unflavored
}

// Read the compact header from the same descriptor whose bytes the caller
// hashed. Keep pkg's complete manifest, including dependencies and future
// fields, and supply the fields a repository adds to an archive's manifest.
func ReadLiveManifest(f *os.File, sum string) (manifest, error) {
	info, err := f.Stat()
	if err != nil {
		return manifest{}, err
	}
	if len(sum) != 64 || !isHex(sum) || sum != strings.ToLower(sum) || !info.Mode().IsRegular() {
		return manifest{}, errors.New("invalid live package archive")
	}
	raw, err := liveCompactJSON(f)
	if err != nil {
		return manifest{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return manifest{}, err
	}
	if fields == nil {
		return manifest{}, errors.New("empty compact manifest")
	}
	path := LiveURLPrefix + sum + ".pkg"
	fields["path"], _ = json.Marshal(path)
	fields["repopath"] = fields["path"]
	fields["sum"], _ = json.Marshal(sum)
	fields["pkgsize"] = json.RawMessage(strconv.FormatInt(info.Size(), 10))
	raw, err = json.Marshal(fields)
	if err != nil {
		return manifest{}, err
	}
	parsed, err := ParseCatalog(raw, "packagesite.yaml", true)
	if err != nil {
		return manifest{}, err
	}
	for _, m := range parsed.ByName {
		p := m.Pkg
		if p.Path == path && p.Sum == sum && p.Size == info.Size() && p.ABI != "" && packageinfo.BuildOrigin.MatchString(p.BuildOrigin) && packageinfo.ValidIssuePackage(p.Name+"-"+p.Version) {
			return m, nil
		}
	}
	return manifest{}, errors.New("invalid package identity in compact manifest")
}

func liveCompactJSON(f *os.File) ([]byte, error) {
	const limit = 4 << 20
	src, err := decompressArchive(io.NewSectionReader(f, 0, limit+512))
	if err != nil {
		return nil, err
	}
	defer src.Close()
	t := tar.NewReader(io.LimitReader(src, limit+512))
	h, err := t.Next()
	if err != nil {
		return nil, err
	}
	if strings.TrimPrefix(h.Name, "./") != "+COMPACT_MANIFEST" || h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > limit {
		return nil, errors.New("missing or invalid +COMPACT_MANIFEST header")
	}
	return io.ReadAll(t)
}

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
func (s *LiveArchiveStore) inheritedBuilds(ctx context.Context, master, all *os.File, current map[string]LivePackage) (map[string]inheritedBuild, error) {
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
		name, version := packageinfo.SplitBuildPackage(id)
		if !ok || name == "" {
			continue
		}
		if p := current[name]; p.File != nil && ComparePkgVersions(p.Manifest.Pkg.Version, version) >= 0 {
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
		t, err := time.Parse(builder.BuildNameFormat, entry.Name())
		if err == nil && t.Format(builder.BuildNameFormat) == entry.Name() && t.Before(s.started) {
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
		run, err := builder.OpenLogPath(master, started.Format(builder.BuildNameFormat), true)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		err = builder.ScanBuildResults(ctx, run, "built", func(_ string, fields []string) bool {
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
func (s *LiveArchiveStore) inspectInherited(all *os.File, name, version, id string) (os.FileInfo, inheritedBuild) {
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
	committed, err := os.Stat(filepath.Join(s.RepoDir, "All", id+".pkg"))
	if err == nil && os.SameFile(info, committed) &&
		(s.offeredVersion == nil || s.versionAlreadyOffered(name, version)) {
		return nil, inheritedBuild{}
	}
	if last := s.inheritedSources[id]; last != nil && sameArchive(last, info) {
		return info, s.inherited[id]
	}
	fetched, err := fetchedByPoudriere(s.RepoDir, id+".pkg", info)
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
	f, err := builder.OpenLogPath(run, "logs/"+id+".log", false)
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
		line = line[len(builder.LogTimePrefix.FindString(line)):]
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
