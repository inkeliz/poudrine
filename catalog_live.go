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
	fetched  bool // supplied from Poudriere's binary package cache
}

type liveHold struct {
	pkg    CatalogPackage
	reason string
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
		return a.fetched == b.fetched && a.manifest.pkg == b.manifest.pkg && a.file != nil && b.file != nil &&
			a.file.path == b.file.path && sameArchive(a.file.info, b.file.info)
	})
	if same && c.localInputsUnchanged(info) {
		return checkUpstreamFingerprint(c.fingerprints, c.revision.fingerprint)
	}
	return c.publishCatalog(ctx, c.upstreamContent, c.revision, index, info, live)
}

type runtimeDependency struct {
	// pkg records the version installed in the build jail here. It resolves
	// ordinary dependencies by name and stores constraints in dep_formula.
	Origin, Version string
}

// Dependencies come from the package's full compact manifest, including on
// paths through committed Poudriere packages or packages supplied by FreeBSD.
type packageRuntime struct {
	Deps     map[string]runtimeDependency
	Requires []string `json:"shlibs_required"`
	Formula  string   `json:"dep_formula"`
	depNames []string
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
	// Report the same first unsatisfied dependency on successive refreshes.
	data.depNames = slices.Sorted(maps.Keys(data.Deps))
	index[m.pkg] = &data
	return &data
}

func (dep runtimeDependency) mismatch(name string, p, consumer CatalogPackage) string {
	switch {
	case p.Name == "":
		return fmt.Sprintf("%s needs %s. The catalog has no package by that name.", consumer.Name, name)
	case dep.Origin != "" && p.Origin != dep.Origin && p.BuildOrigin != dep.Origin:
		return fmt.Sprintf("%s needs %s from %s. The catalog offers it from %s.", consumer.Name, name, dep.Origin, p.BuildOrigin)
	case !compatibleABI(consumer.ABI, p.ABI):
		return fmt.Sprintf("%s needs %s for %s. The catalog offers it for %s.", consumer.Name, name, consumer.ABI, p.ABI)
	default:
		return ""
	}
}

// pkg can choose a shared library provider independently of named deps.
// Index exactly the manifests an install would see in this publication pass.
type libraryProviders map[string][]string

func offeredLibraries(selected map[string]manifest, live map[string]livePackage) libraryProviders {
	index := make(libraryProviders)
	add := func(m manifest) {
		for _, lib := range m.provides {
			if !slices.Contains(index[lib], m.pkg.ABI) {
				index[lib] = append(index[lib], m.pkg.ABI)
			}
		}
	}
	for name, m := range selected {
		if candidate, ok := live[name]; ok && preferLive(candidate.manifest.pkg, m.pkg) {
			continue
		}
		add(m)
	}
	for name, candidate := range live {
		if preferLive(candidate.manifest.pkg, selected[name].pkg) {
			add(candidate.manifest)
		}
	}
	return index
}

func (index libraryProviders) has(lib, abi string) bool {
	return slices.ContainsFunc(index[lib], func(provider string) bool { return compatibleABI(provider, abi) })
}

type liveLibraries struct {
	base    map[string]bool
	staged  libraryProviders
	offered libraryProviders
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
func (index dependencyCache) hold(m manifest, available map[string]manifest, live map[string]livePackage, libs liveLibraries, held map[string]liveHold) (reason string, waitForFallback bool) {
	stack := []manifest{m}
	visited := map[CatalogPackage]bool{m.pkg: true}
	for len(stack) != 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		runtime := index.read(current)
		if runtime == nil {
			return fmt.Sprintf("Cannot read the runtime dependencies of %s.", current.pkg.Name), false
		}
		for _, name := range runtime.depNames {
			dep := runtime.Deps[name]
			provider := available[name]
			staged, exists := live[name]
			if exists && preferLive(staged.manifest.pkg, provider.pkg) {
				if mismatch := dep.mismatch(name, staged.manifest.pkg, current.pkg); mismatch != "" {
					if dep.mismatch(name, provider.pkg, current.pkg) != "" {
						return mismatch, false
					}
					// A port move can change an origin. Wait for the
					// replacement's own dependencies to be checked first.
					reason, waitForFallback = mismatch, true
				}
				continue
			}
			if mismatch := dep.mismatch(name, provider.pkg, current.pkg); mismatch != "" {
				// The matching staged dependency may itself be waiting for a
				// library several levels down. Show that underlying blocker.
				if h, ok := held[name]; ok && dep.mismatch(name, h.pkg, current.pkg) == "" {
					return h.reason, false
				}
				return mismatch, false
			}
			if !visited[provider.pkg] {
				visited[provider.pkg] = true
				stack = append(stack, provider)
			}
		}
		for _, lib := range runtime.Requires {
			// Check every requirement of the new build. For a dependency
			// already in the published catalog, revisit libraries whose
			// provider participates in this bulk.
			if current.pkg != m.pkg && !libs.staged.has(lib, current.pkg.ABI) {
				continue
			}
			if !libs.base[lib] && !libs.offered.has(lib, current.pkg.ABI) {
				return fmt.Sprintf("%s needs %s. The repository and the build jail have no provider for %s.", current.pkg.Name, lib, current.pkg.ABI), false
			}
		}
	}
	return reason, waitForFallback
}

func mergeLivePackages(live map[string]livePackage, selected map[string]manifest, arches, baseLibraries map[string]bool, s *catalogSnapshot) {
	eligible := maps.Clone(live)
	deps := make(dependencyCache)
	held := make(map[string]liveHold)
	hold := func(name, reason string) {
		held[name] = liveHold{pkg: eligible[name].manifest.pkg, reason: reason}
		delete(eligible, name)
	}
	for name, candidate := range eligible {
		p, current := candidate.manifest.pkg, selected[name].pkg
		switch {
		case name != p.Name || !p.Local || candidate.file == nil:
			hold(name, "The captured archive does not match the local build.")
			continue
		case !arches[p.ABI]:
			hold(name, fmt.Sprintf("The repository does not contain packages for %s.", p.ABI))
			continue
		case current.Name != "" && !compatibleABI(p.ABI, current.ABI):
			hold(name, fmt.Sprintf("This build targets %s. The catalog offers %s for %s.", p.ABI, name, current.ABI))
			continue
		case candidate.fetched && current.Name != "" && comparePkgVersions(p.Version, current.Version) <= 0:
			// Keep the normal signed-upstream or committed download on a tie.
			delete(eligible, name)
			continue
		case !preferLive(p, s.compiled[name]):
			compiled := s.compiled[name]
			hold(name, fmt.Sprintf("The committed repository already contains %s %s from %s.", name, compiled.Version, compiled.BuildOrigin))
			continue
		}
		if err := candidate.file.verify(p); err != nil {
			hold(name, "The captured archive failed verification.")
			continue
		}
		next := deps.read(candidate.manifest)
		if next == nil {
			hold(name, fmt.Sprintf("Cannot read the runtime dependencies of %s.", name))
			continue
		}
		if next.Formula != "" {
			hold(name, fmt.Sprintf("%s declares a dependency formula (%s). Waiting for Poudriere to commit the bulk repository.", name, next.Formula))
			continue
		}
		if current.Name != "" && preferLive(p, current) {
			// Existing installed consumers may still need any soname exposed
			// by the selected provider. Change those together at bulk commit.
			for _, lib := range selected[name].provides {
				if !slices.Contains(candidate.manifest.provides, lib) {
					hold(name, fmt.Sprintf("%s removes %s. Waiting for Poudriere to commit the bulk repository.", name, lib))
					break
				}
			}
		}
	}
	libs := liveLibraries{base: baseLibraries, staged: offeredLibraries(nil, live)}
	for len(eligible) > 0 {
		libs.offered = offeredLibraries(selected, eligible)
		blocked, waiting := make(map[string]string), make(map[string]string)
		for name, candidate := range eligible {
			reason, fallback := deps.hold(candidate.manifest, selected, eligible, libs, held)
			if fallback {
				waiting[name] = reason
			} else if reason != "" {
				blocked[name] = reason
			}
		}
		if len(blocked) == 0 {
			blocked = waiting
		}
		if len(blocked) == 0 {
			break
		}
		for name, reason := range blocked {
			hold(name, reason)
		}
	}
	if len(held) != 0 {
		s.liveHolds = make(map[string]liveHold, len(held))
		for _, h := range held {
			s.liveHolds[h.pkg.Name+"-"+h.pkg.Version] = h
		}
	}
	for name, candidate := range eligible {
		m := candidate.manifest
		m.pkg.live = true
		if !candidate.fetched {
			s.compiled[name], s.compiledFiles[name], s.compiledAt[name] = m.pkg, candidate.file, candidate.file.info.ModTime()
		}
		current := selected[name].pkg
		if !preferLive(m.pkg, current) {
			continue
		}
		if current.Name != "" && !current.Local {
			s.displaced[name] = current
		}
		if candidate.fetched {
			s.fetched[name] = true
		} else {
			delete(s.fetched, name)
		}
		if s.beforeLive == nil {
			s.beforeLive = make(map[string]string)
		}
		s.beforeLive[name] = current.Version
		selected[name] = m
	}
}
