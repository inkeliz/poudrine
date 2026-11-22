package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/packageinfo"
)

// Native pkg fetch records the origin and package ID after linking its cache
// entry into All. A retained stage can contain downloads from an earlier run.
type fetchResults map[string][]string

func (results fetchResults) read(ctx context.Context, run *os.File) error {
	return builder.ScanBuildResults(ctx, run, "fetched", func(_ string, fields []string) bool {
		id, origin := fields[1], fields[0]
		if !slices.Contains(results[id], origin) {
			results[id] = append(results[id], origin)
		}
		return true
	})
}

func (s *LiveArchiveStore) priorFetchResults(ctx context.Context, master *os.File) (fetchResults, error) {
	if s.priorFetches != nil {
		return s.priorFetches, nil
	}
	if _, err := master.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	entries, err := master.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	results := make(fetchResults)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		started, err := time.Parse(builder.BuildNameFormat, entry.Name())
		if !entry.IsDir() || err != nil || started.Format(builder.BuildNameFormat) != entry.Name() || !started.Before(s.started) {
			continue
		}
		run, err := builder.OpenLogPath(master, entry.Name(), true)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		err = results.read(ctx, run)
		run.Close()
		if err != nil {
			return nil, err
		}
	}
	s.priorFetches = results
	return results, nil
}

type fetchDependency struct {
	name, abi string
	runtimeDependency
}

func (s *LiveArchiveStore) addFetchedDependencies(ctx context.Context, master, run, all *os.File, next map[string]LivePackage) error {
	if len(next) == 0 {
		return nil
	}
	cacheDir := filepath.Join(s.RepoDir, ".pkg-cache")
	info, err := os.Lstat(cacheDir)
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.IsDir() {
		return nil
	}
	if err != nil {
		return err
	}
	cache, err := os.OpenRoot(cacheDir)
	if err != nil {
		return err
	}
	defer cache.Close()
	results := make(fetchResults)
	if err := results.read(ctx, run); err != nil {
		return err
	}

	seen := make(map[fetchDependency]bool)
	var pending []fetchDependency
	add := func(abi string, deps map[string]runtimeDependency) {
		for name, dep := range deps {
			req := fetchDependency{name, abi, dep}
			if dep.Version != "" && !seen[req] {
				seen[req] = true
				pending = append(pending, req)
			}
		}
	}
	for _, root := range next {
		var runtime packageRuntime
		if json.Unmarshal(root.Manifest.Raw, &runtime) == nil {
			add(root.Manifest.Pkg.ABI, runtime.Deps)
		}
	}
	var firstErr error
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		req := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		id := req.name + "-" + req.Version
		name, version := packageinfo.SplitBuildPackage(id)
		if name != req.name || version != req.Version {
			continue
		}
		if selected := next[name]; selected.File != nil && ComparePkgVersions(selected.Manifest.Pkg.Version, version) >= 0 {
			continue
		}
		child, abi, deps, err := s.fetchedDependency(ctx, master, all, cache, results, req, id)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", id, err)
			}
			continue
		}
		if child.File != nil {
			next[name] = child
		}
		add(abi, deps)
	}
	return firstErr
}

// pkg's cache can name the data directly, or use a single relative alias
// from name-version.pkg to name-version~digest.pkg in the same directory.
func nativeCachedArchive(cache *os.Root, id string) (os.FileInfo, error) {
	name := id + ".pkg"
	info, err := cache.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return info, err
	}
	target, err := cache.Readlink(name)
	if err != nil {
		return nil, err
	}
	if !isHashedArchive(target, id) {
		return nil, errors.New("unexpected native package cache alias")
	}
	return cache.Lstat(target)
}

func (s *LiveArchiveStore) fetchedDependency(ctx context.Context, master, all *os.File, cache *os.Root, results fetchResults, req fetchDependency, id string) (LivePackage, string, map[string]runtimeDependency, error) {
	var none LivePackage
	source, err := openStageArchive(all, id)
	if err != nil {
		return none, "", nil, err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return none, "", nil, err
	}
	cached, err := nativeCachedArchive(cache, id)
	if err != nil {
		return none, "", nil, err
	}
	if !info.Mode().IsRegular() || !cached.Mode().IsRegular() || !os.SameFile(info, cached) {
		return none, "", nil, nil
	}
	var header struct {
		Name, Origin, Version, ABI, Arch string
		Annotations                      struct{ Flavor, Subpackage string }
		Deps                             map[string]runtimeDependency
	}
	raw, err := liveCompactJSON(source)
	if err != nil {
		return none, "", nil, err
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return none, "", nil, err
	}
	p := packageinfo.Package{Name: header.Name, Version: header.Version, Origin: header.Origin,
		BuildOrigin: packageinfo.PackageBuildOrigin(header.Origin, header.Annotations.Flavor, header.Annotations.Subpackage), ABI: packageinfo.PackageABI(header.ABI, header.Arch)}
	if p.Name != req.name || p.Version != req.Version || req.mismatch(req.name, p, packageinfo.Package{ABI: req.abi}) != "" {
		return none, "", nil, nil
	}
	matches := func(origin string) bool { return matchesResultOrigin(p, origin) }
	if !slices.ContainsFunc(results[id], matches) {
		prior, err := s.priorFetchResults(ctx, master)
		if err != nil {
			return none, "", nil, err
		}
		if !slices.ContainsFunc(prior[id], matches) {
			return none, "", nil, nil
		}
	}
	// Follow this intermediary's exact dependency records even if its own
	// version is already available; a dependency further down may be newer.
	if s.versionAlreadyOffered(req.name, req.Version) {
		return none, p.ABI, header.Deps, nil
	}
	old := s.packages[p.Name]
	if old.Fetched && old.File != nil && old.Manifest.Pkg.Version == p.Version && s.sources[id] != nil && sameArchive(s.sources[id], info) {
		return old, p.ABI, header.Deps, nil
	}
	entry, err := s.capture(ctx, source, info, id, p.BuildOrigin, time.Time{})
	if err != nil {
		return none, "", nil, err
	}
	if entry.Manifest.Pkg.ABI != p.ABI {
		return none, "", nil, fmt.Errorf("archive ABI changed while capturing %s", id)
	}
	entry.Fetched = true
	s.sources[id] = info
	return entry, p.ABI, header.Deps, nil
}
