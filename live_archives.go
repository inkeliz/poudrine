package poudrine

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
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The native stage and its marker identify a run even when the worker has
// restarted. A successful log entry alone cannot publish a package.
type liveArchiveStore struct {
	dir, repoDir     string
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
	packages         map[string]livePackage
	failures         map[string]liveCaptureFailure
}

type liveCaptureFailure struct {
	origin  string
	message string
	retry   time.Time
}

func (s *liveArchiveStore) reset(stage os.FileInfo, started time.Time) {
	*s = liveArchiveStore{dir: s.dir, repoDir: s.repoDir, offeredVersion: s.offeredVersion, stage: stage, started: started,
		packages: make(map[string]livePackage), sources: make(map[string]os.FileInfo), failures: make(map[string]liveCaptureFailure)}
}

func stageBuildName(stage *os.File) (time.Time, error) {
	marker, err := openLogPath(stage, ".buildname", false)
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
	t, err := time.Parse(buildNameFormat, name)
	if err != nil || name != t.Format(buildNameFormat) {
		return time.Time{}, errors.New("stage has no Poudrine build name")
	}
	return t, nil
}

func (s *liveArchiveStore) scan(ctx context.Context, cfg BuilderConfig) (map[string]livePackage, error) {
	path := filepath.Join(s.repoDir, ".building")
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
	if committed, err := os.OpenFile(filepath.Join(s.repoDir, ".latest"), os.O_RDONLY|syscall.O_DIRECTORY, 0); err == nil {
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
	logs, err := openBulkLogs(cfg)
	if err != nil {
		return nil, err
	}
	defer logs.Close()
	master, err := openLogPath(logs, cfg.Jail+"-"+cfg.Ports, true)
	if err != nil {
		return nil, err
	}
	defer master.Close()
	run, err := openLogPath(master, started.Format(buildNameFormat), true)
	if err != nil {
		return nil, err
	}
	defer run.Close()
	all, err := openLogPath(stage, "All", true)
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
	logDir := filepath.Join(cfg.BaseFS, "data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports, started.Format(buildNameFormat))
	for i, path := range []string{filepath.Join(path, "All", "Hashed"), filepath.Join(logDir, ".poudriere.ports.built"), filepath.Join(logDir, ".poudriere.ports.fetched"), filepath.Join(s.repoDir, ".pkg-cache")} {
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
	next := make(map[string]livePackage, len(s.packages))
	s.offered = make(map[string]string)
	results := make(map[string]bool)
	visit := func(id, origin string, stamp time.Time) {
		results[id] = true
		name, version := splitBuildPackage(id)
		if old := next[name]; old.file != nil && comparePkgVersions(old.manifest.pkg.Version, version) >= 0 {
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
		if p.file != nil {
			next[p.manifest.pkg.Name] = p
		}
	}
	if err := scanBuildResults(ctx, run, "built", func(_ string, fields []string) bool {
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

func (s *liveArchiveStore) unchanged(inputs [5]os.FileInfo) bool {
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
		if baseline := s.offeredVersion(name); baseline == "" || comparePkgVersions(baseline, version) < 0 {
			return false
		}
	}
	return true
}

func (s *liveArchiveStore) versionAlreadyOffered(name, version string) bool {
	if s.offeredVersion == nil {
		return false
	}
	baseline := s.offeredVersion(name)
	if baseline == "" || comparePkgVersions(baseline, version) < 0 {
		return false
	}
	if old := s.offered[name]; old == "" || comparePkgVersions(version, old) > 0 {
		s.offered[name] = version
	}
	return true
}

// pkg repo --hash --symlink puts a relative alias at All/<pkg>.pkg.
// Resolve that one native layout descriptor-relatively, rejecting any other
// target or nested symlink. The manifest and complete archive are checked next.
func openStageArchive(all *os.File, id string) (*os.File, error) {
	f, err := openLogPath(all, id+".pkg", false)
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
	return openLogPath(all, target, false)
}

func isHashedArchive(name, id string) bool {
	suffix, ok := strings.CutPrefix(name, id+"~")
	hash, ext := strings.CutSuffix(suffix, ".pkg")
	return ok && ext && hash != "" && strings.Trim(hash, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ$") == ""
}

func (s *liveArchiveStore) stagedPackage(ctx context.Context, all *os.File, id, origin string, stamp time.Time) (livePackage, error) {
	name, version := splitBuildPackage(id)
	if name == "" || time.Now().Before(s.failures[id].retry) {
		return livePackage{}, nil
	}
	source, err := openStageArchive(all, id)
	if err != nil {
		return livePackage{}, err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return livePackage{}, err
	}
	fetched, err := fetchedByPoudriere(s.repoDir, id+".pkg", info)
	if err != nil {
		return livePackage{}, err
	}
	if fetched {
		return livePackage{}, errors.New("staged archive belongs to Poudriere's download cache")
	}
	if old := s.packages[name]; !old.fetched && old.manifest.pkg.Version == version && old.file != nil && s.sources[id] != nil && sameArchive(s.sources[id], info) {
		return old, nil
	}
	p, err := s.capture(ctx, source, info, id, origin, stamp)
	if err == nil {
		s.sources[id] = info
		delete(s.failures, id)
	}
	return p, err
}

func (s *liveArchiveStore) capture(ctx context.Context, source *os.File, before os.FileInfo, id, origin string, stamp time.Time) (livePackage, error) {
	if !before.Mode().IsRegular() || before.Size() <= 0 {
		return livePackage{}, errors.New("unexpected staged package file")
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return livePackage{}, err
	}
	tmp, err := os.CreateTemp(s.dir, ".capture-")
	if err != nil {
		return livePackage{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	stop := context.AfterFunc(ctx, func() { source.Close() })
	defer stop()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(source, before.Size()))
	if err != nil {
		return livePackage{}, err
	}
	if err := ctx.Err(); err != nil {
		return livePackage{}, err
	}
	after, err := source.Stat()
	if err != nil {
		return livePackage{}, err
	}
	if n != before.Size() || !sameArchive(before, after) {
		return livePackage{}, errors.New("staged package changed while copying")
	}
	sum := hex.EncodeToString(h.Sum(nil))
	m, err := readLiveManifest(tmp, sum)
	if err != nil {
		return livePackage{}, err
	}
	// The result list can omit the default flavor, including when it names
	// a subpackage. Honor every flavor and subpackage it explicitly names.
	if m.pkg.Name+"-"+m.pkg.Version != id || !matchesResultOrigin(m.pkg, origin) {
		return livePackage{}, fmt.Errorf("compact manifest does not match successful build %s %s", origin, id)
	}

	if !stamp.IsZero() && !archiveBuildTime(m.raw).Equal(stamp) {
		return livePackage{}, errors.New("retained archive differs from recorded build timestamp")
	}
	dest := filepath.Join(s.dir, sum+".pkg")
	// Link only validated bytes into the addressable namespace. A prior run
	// may already own this path; verify those bytes before reusing the inode.
	if err := os.Link(tmp.Name(), dest); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return livePackage{}, err
		}
		old, err := os.Lstat(dest)
		if err != nil {
			return livePackage{}, err
		}
		file := &compiledArchive{path: dest, info: old}
		if old.Mode().IsRegular() && file.verify(m.pkg) == nil {
			return livePackage{manifest: m, file: file}, nil
		}
		if err := os.Rename(tmp.Name(), dest); err != nil {
			return livePackage{}, err
		}
	}
	info, err := tmp.Stat()
	if err != nil {
		return livePackage{}, err
	}
	return livePackage{manifest: m, file: &compiledArchive{path: dest, info: info, verified: true}}, nil
}

func matchesResultOrigin(p CatalogPackage, origin string) bool {
	unflavored := p.Origin
	if _, sub, ok := strings.Cut(p.BuildOrigin, "~"); ok {
		unflavored += "~" + sub
	}
	return origin == p.BuildOrigin || origin == unflavored
}

// Read the compact header from the same descriptor whose bytes the caller
// hashed. Keep pkg's complete manifest, including dependencies and future
// fields, and supply the fields a repository adds to an archive's manifest.
func readLiveManifest(f *os.File, sum string) (manifest, error) {
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
	path := liveURLPrefix + sum + ".pkg"
	fields["path"], _ = json.Marshal(path)
	fields["repopath"] = fields["path"]
	fields["sum"], _ = json.Marshal(sum)
	fields["pkgsize"] = json.RawMessage(strconv.FormatInt(info.Size(), 10))
	raw, err = json.Marshal(fields)
	if err != nil {
		return manifest{}, err
	}
	parsed, err := parseCatalog(raw, "packagesite.yaml", true)
	if err != nil {
		return manifest{}, err
	}
	for _, m := range parsed.byName {
		p := m.pkg
		if p.Path == path && p.Sum == sum && p.Size == info.Size() && p.ABI != "" && buildOrigin.MatchString(p.BuildOrigin) && validIssuePackage(p.Name+"-"+p.Version) {
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
