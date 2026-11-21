package poudrine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

type livePackage struct {
	manifest manifest
	file     *compiledArchive
}

// PublishLive replaces the set of successful packages staged by the worker.
// A local publication can use the last authenticated upstream catalog even
// while a scheduled remote refresh is in flight.
func (c *Catalog) PublishLive(ctx context.Context, live map[string]livePackage) error {
	c.publish.Lock()
	defer c.publish.Unlock()
	if c.upstreamContent == nil {
		return errors.New("waiting for an authenticated upstream catalog")
	}
	index, info, err := c.openLocalIndex()
	if err != nil {
		return fmt.Errorf("open local Poudriere catalog: %w", err)
	}
	if index != nil {
		defer index.Close()
	}
	same := maps.EqualFunc(live, c.live, func(a, b livePackage) bool {
		// A staged URL identifies immutable bytes including the manifest.
		return a.manifest.pkg == b.manifest.pkg && a.file != nil && b.file != nil &&
			a.file.path == b.file.path && sameArchive(a.file.info, b.file.info)
	})
	if same && c.localInputsUnchanged(info) {
		return checkUpstreamFingerprint(c.fingerprints, c.revision.fingerprint)
	}
	return c.publishCatalog(ctx, c.upstreamContent, c.revision, index, info, live)
}

type runtimeDependency struct {
	Origin, Version string
}

// Dependencies come from the package's full compact manifest, including on
// paths through committed Poudriere packages or packages supplied by FreeBSD.
type packageRuntime struct {
	Deps     map[string]runtimeDependency
	Requires []string `json:"shlibs_required"`
	Provides []string `json:"shlibs_provided"`
}

type dependencyCache map[CatalogPackage]*packageRuntime

func (index dependencyCache) read(m manifest) *packageRuntime {
	if data, ok := index[m.pkg]; ok {
		return data
	}
	var data packageRuntime
	if err := json.Unmarshal(m.raw, &data); err != nil {
		index[m.pkg] = nil
		return nil
	}
	index[m.pkg] = &data
	return &data
}

func (dep runtimeDependency) accepts(p CatalogPackage, abi string) bool {
	return p.Name != "" && dep.Origin != "" && (p.Origin == dep.Origin || p.BuildOrigin == dep.Origin) &&
		compatibleABI(abi, p.ABI) && (dep.Version == "" || comparePkgVersions(p.Version, dep.Version) >= 0)
}

func preferLive(p, current CatalogPackage) bool {
	if current.Name == "" {
		return true
	}
	order := comparePkgVersions(p.Version, current.Version)
	return order > 0 || order == 0 && p.BuildOrigin == current.BuildOrigin
}

// A dependency satisfied by another selected live package is checked in that
// package's own turn. If it cannot be published, the next pass checks its
// dependents against the remaining catalog, which can include a usable older
// build. Cycles with all their required archives present are valid.
func (index dependencyCache) ready(m manifest, available map[string]manifest, live map[string]livePackage, newLibraries map[string][]string) (ready, waitingForFallback bool) {
	stack := []manifest{m}
	visited := map[CatalogPackage]bool{m.pkg: true}
	for len(stack) != 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		runtime := index.read(current)
		if runtime == nil {
			return false, false
		}
		for name, dep := range runtime.Deps {
			provider := available[name]
			staged, exists := live[name]
			if exists && preferLive(staged.manifest.pkg, provider.pkg) {
				if !dep.accepts(staged.manifest.pkg, current.pkg.ABI) {
					if !dep.accepts(provider.pkg, current.pkg.ABI) {
						return false, false
					}
					// A port move can change an origin. Wait for the
					// replacement's own dependencies to be checked first.
					waitingForFallback = true
				}
				continue
			}
			if !dep.accepts(provider.pkg, current.pkg.ABI) {
				return false, false
			}
			// An ABI rebuild can retain the same package version. The old
			// provider cannot supply a library first added by the staged one.
			for _, lib := range newLibraries[name] {
				if slices.Contains(runtime.Requires, lib) {
					return false, false
				}
			}
			if !visited[provider.pkg] {
				visited[provider.pkg] = true
				stack = append(stack, provider)
			}
		}
	}
	return !waitingForFallback, waitingForFallback
}

func mergeLivePackages(live map[string]livePackage, selected map[string]manifest, arches map[string]bool, s *catalogSnapshot) {
	eligible := maps.Clone(live)
	deps := make(dependencyCache)
	newLibraries := make(map[string][]string)
	for name, candidate := range eligible {
		p, current := candidate.manifest.pkg, selected[name].pkg
		if name != p.Name || !p.Local || !arches[p.ABI] || current.Name != "" && !compatibleABI(p.ABI, current.ABI) ||
			!preferLive(p, s.compiled[name]) || candidate.file == nil || candidate.file.verify(p) != nil {
			delete(eligible, name)
			continue
		}
		if current.Name != "" && preferLive(p, current) {
			old, next := deps.read(selected[name]), deps.read(candidate.manifest)
			if old == nil || next == nil {
				delete(eligible, name)
				continue
			}
			for _, lib := range next.Provides {
				if !slices.Contains(old.Provides, lib) {
					newLibraries[name] = append(newLibraries[name], lib)
				}
			}
			// Existing installed consumers may still need any soname exposed
			// by the selected provider. Change those together at bulk commit.
			for _, lib := range old.Provides {
				if !slices.Contains(next.Provides, lib) {
					delete(eligible, name)
					break
				}
			}
		}
	}
	for {
		var blocked, waiting []string
		for name, candidate := range eligible {
			ready, fallback := deps.ready(candidate.manifest, selected, eligible, newLibraries)
			if fallback {
				waiting = append(waiting, name)
			} else if !ready {
				blocked = append(blocked, name)
			}
		}
		if len(blocked) == 0 {
			blocked = waiting
		}
		if len(blocked) == 0 {
			break
		}
		for _, name := range blocked {
			delete(eligible, name)
		}
	}
	for name, candidate := range eligible {
		m := candidate.manifest
		m.pkg.live = true
		s.compiled[name], s.compiledFiles[name], s.compiledAt[name] = m.pkg, candidate.file, candidate.file.info.ModTime()
		current := selected[name].pkg
		if !preferLive(m.pkg, current) {
			continue
		}
		if current.Name != "" && !current.Local {
			s.displaced[name] = current
		}
		delete(s.fetched, name)
		selected[name] = m
	}
}
