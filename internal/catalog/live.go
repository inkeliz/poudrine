package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/inkeliz/poudrine/internal/packageinfo"
)

type LivePackage struct {
	Manifest manifest         `json:"-"`
	File     *CompiledArchive `json:"-"`
	Fetched  bool             `json:"-"` // supplied from Poudriere's binary package cache
}

type liveHold struct {
	pkg    packageinfo.Package
	Reason string `json:"-"`
}

// PublishLive replaces the set of successful packages staged by the worker.
// A local publication can use the last authenticated upstream catalog even
// while a scheduled remote refresh is in flight.
func (c *Catalog) PublishLive(ctx context.Context, live map[string]LivePackage) error {
	c.publish.Lock()
	defer c.publish.Unlock()
	if c.upstreamContent == nil {
		return errors.New("waiting for an authenticated upstream catalog")
	}
	index, info, err := c.OpenLocalIndex()
	if err != nil {
		return fmt.Errorf("open local Poudriere catalog: %w", err)
	}
	if index != nil {
		defer index.Close()
	}
	same := maps.EqualFunc(live, c.Live, func(a, b LivePackage) bool {
		// A staged URL identifies immutable bytes including the manifest.
		return a.Fetched == b.Fetched && a.Manifest.Pkg == b.Manifest.Pkg && a.File != nil && b.File != nil &&
			a.File.Path == b.File.Path && sameArchive(a.File.Info, b.File.Info)
	})
	if same && c.localInputsUnchanged(info) {
		return checkUpstreamFingerprint(c.fingerprints, c.revision.Fingerprint)
	}
	return c.PublishCatalog(ctx, c.upstreamContent, c.revision, index, info, live)
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

type dependencyCache map[packageinfo.Package]*packageRuntime

func (index dependencyCache) read(m manifest) *packageRuntime {
	if data, ok := index[m.Pkg]; ok {
		return data
	}
	var data packageRuntime
	if err := json.Unmarshal(m.Raw, &data); err != nil {
		index[m.Pkg] = nil
		return nil
	}
	// Report the same first unsatisfied dependency on successive refreshes.
	data.depNames = slices.Sorted(maps.Keys(data.Deps))
	index[m.Pkg] = &data
	return &data
}

func (dep runtimeDependency) mismatch(name string, p, consumer packageinfo.Package) string {
	switch {
	case p.Name == "":
		return fmt.Sprintf("%s needs %s. The catalog has no package by that name.", consumer.Name, name)
	case dep.Origin != "" && p.Origin != dep.Origin && p.BuildOrigin != dep.Origin:
		return fmt.Sprintf("%s needs %s from %s. The catalog offers it from %s.", consumer.Name, name, dep.Origin, p.BuildOrigin)
	case !packageinfo.CompatibleABI(consumer.ABI, p.ABI):
		return fmt.Sprintf("%s needs %s for %s. The catalog offers it for %s.", consumer.Name, name, consumer.ABI, p.ABI)
	default:
		return ""
	}
}

// pkg can choose a shared library provider independently of named deps.
// Index exactly the manifests an install would see in this publication pass.
type libraryProviders map[string][]string

func offeredLibraries(selected map[string]manifest, live map[string]LivePackage) libraryProviders {
	index := make(libraryProviders)
	add := func(m manifest) {
		for _, lib := range m.provides {
			if !slices.Contains(index[lib], m.Pkg.ABI) {
				index[lib] = append(index[lib], m.Pkg.ABI)
			}
		}
	}
	for name, m := range selected {
		if candidate, ok := live[name]; ok && preferLive(candidate.Manifest.Pkg, m.Pkg) {
			continue
		}
		add(m)
	}
	for name, candidate := range live {
		if preferLive(candidate.Manifest.Pkg, selected[name].Pkg) {
			add(candidate.Manifest)
		}
	}
	return index
}

func (index libraryProviders) has(lib, abi string) bool {
	return slices.ContainsFunc(index[lib], func(provider string) bool { return packageinfo.CompatibleABI(provider, abi) })
}

type liveLibraries struct {
	base    map[string]bool
	staged  libraryProviders
	offered libraryProviders
}

func preferLive(p, current packageinfo.Package) bool {
	if current.Name == "" {
		return true
	}
	order := ComparePkgVersions(p.Version, current.Version)
	return order > 0 || order == 0 && p.BuildOrigin == current.BuildOrigin
}

// A dependency satisfied by another selected live package is checked in that
// package's own turn. If it cannot be published, the next pass checks its
// dependents against the remaining catalog, which can include a usable older
// build. Cycles with all their required archives present are valid.
func (index dependencyCache) hold(m manifest, available map[string]manifest, live map[string]LivePackage, libs liveLibraries, held map[string]liveHold) (reason string, waitForFallback bool) {
	stack := []manifest{m}
	visited := map[packageinfo.Package]bool{m.Pkg: true}
	for len(stack) != 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		runtime := index.read(current)
		if runtime == nil {
			return fmt.Sprintf("Cannot read the runtime dependencies of %s.", current.Pkg.Name), false
		}
		for _, name := range runtime.depNames {
			dep := runtime.Deps[name]
			provider := available[name]
			staged, exists := live[name]
			if exists && preferLive(staged.Manifest.Pkg, provider.Pkg) {
				if mismatch := dep.mismatch(name, staged.Manifest.Pkg, current.Pkg); mismatch != "" {
					if dep.mismatch(name, provider.Pkg, current.Pkg) != "" {
						return mismatch, false
					}
					// A port move can change an origin. Wait for the
					// replacement's own dependencies to be checked first.
					reason, waitForFallback = mismatch, true
				}
				continue
			}
			if mismatch := dep.mismatch(name, provider.Pkg, current.Pkg); mismatch != "" {
				// The matching staged dependency may itself be waiting for a
				// library several levels down. Show that underlying blocker.
				if h, ok := held[name]; ok && dep.mismatch(name, h.pkg, current.Pkg) == "" {
					return h.Reason, false
				}
				return mismatch, false
			}
			if !visited[provider.Pkg] {
				visited[provider.Pkg] = true
				stack = append(stack, provider)
			}
		}
		for _, lib := range runtime.Requires {
			// Check every requirement of the new build. For a dependency
			// already in the published catalog, revisit libraries whose
			// provider participates in this bulk.
			if current.Pkg != m.Pkg && !libs.staged.has(lib, current.Pkg.ABI) {
				continue
			}
			if !libs.base[lib] && !libs.offered.has(lib, current.Pkg.ABI) {
				return fmt.Sprintf("%s needs %s. The repository and the build jail have no provider for %s.", current.Pkg.Name, lib, current.Pkg.ABI), false
			}
		}
	}
	return reason, waitForFallback
}

func mergeLivePackages(live map[string]LivePackage, selected map[string]manifest, arches, baseLibraries map[string]bool, s *Snapshot) {
	eligible := maps.Clone(live)
	deps := make(dependencyCache)
	held := make(map[string]liveHold)
	hold := func(name, reason string) {
		held[name] = liveHold{pkg: eligible[name].Manifest.Pkg, Reason: reason}
		delete(eligible, name)
	}
	for name, candidate := range eligible {
		p, current := candidate.Manifest.Pkg, selected[name].Pkg
		switch {
		case name != p.Name || !p.Local || candidate.File == nil:
			hold(name, "The captured archive does not match the local build.")
			continue
		case !s.PermitsLocal(p, candidate.Fetched):
			hold(name, "Waiting for a local build with the current package settings.")
			continue
		case !arches[p.ABI]:
			hold(name, fmt.Sprintf("The repository does not contain packages for %s.", p.ABI))
			continue
		case current.Name != "" && !packageinfo.CompatibleABI(p.ABI, current.ABI):
			hold(name, fmt.Sprintf("This build targets %s. The catalog offers %s for %s.", p.ABI, name, current.ABI))
			continue
		case candidate.Fetched && current.Name != "" && ComparePkgVersions(p.Version, current.Version) <= 0:
			// Keep the normal signed-upstream or committed download on a tie.
			delete(eligible, name)
			continue
		case !preferLive(p, s.Compiled[name]):
			compiled := s.Compiled[name]
			hold(name, fmt.Sprintf("The committed repository already contains %s %s from %s.", name, compiled.Version, compiled.BuildOrigin))
			continue
		}
		if err := candidate.File.verify(p); err != nil {
			hold(name, "The captured archive failed verification.")
			continue
		}
		next := deps.read(candidate.Manifest)
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
				if !slices.Contains(candidate.Manifest.provides, lib) {
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
			reason, fallback := deps.hold(candidate.Manifest, selected, eligible, libs, held)
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
		s.LiveHolds = make(map[string]liveHold, len(held))
		for _, h := range held {
			s.LiveHolds[h.pkg.Name+"-"+h.pkg.Version] = h
		}
	}
	for name, candidate := range eligible {
		m := candidate.Manifest
		m.Pkg.Live = true
		if !candidate.Fetched {
			s.Compiled[name], s.compiledFiles[name], s.compiledAt[name] = m.Pkg, candidate.File, candidate.File.Info.ModTime()
		}
		current := selected[name].Pkg
		if !preferLive(m.Pkg, current) {
			continue
		}
		if current.Name != "" && !current.Local {
			s.Displaced[name] = current
		}
		if candidate.Fetched {
			s.Fetched[name] = true
		} else {
			delete(s.Fetched, name)
		}
		if s.beforeLive == nil {
			s.beforeLive = make(map[string]string)
		}
		s.beforeLive[name] = current.Version
		selected[name] = m
	}
}
