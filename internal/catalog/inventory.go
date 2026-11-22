package catalog

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/packageinfo"
)

type PackageInventoryView struct {
	Packages       []managementPackage `json:"packages"`
	CachedCount    int                 `json:"cached_count"`
	CachedBytes    int64               `json:"cached_bytes"`
	CompiledBytes  *int64              `json:"compiled_bytes"`
	PoudriereBytes *int64              `json:"poudriere_bytes"`
	CacheLimit     int64               `json:"cache_limit"`
	TrackedOrigins int                 `json:"tracked_origins"`
	CatalogReady   bool                `json:"catalog_ready"`
	OriginsReady   bool                `json:"origins_ready"`
}

type managementPackage struct {
	Name            string    `json:"name"`
	Origin          string    `json:"origin"`
	BuildOrigin     string    `json:"build_origin"`
	Source          string    `json:"source"`
	Version         string    `json:"version,omitempty"`
	PortVersion     string    `json:"port_version,omitempty"`
	LocalVersion    string    `json:"local_version,omitempty"`
	FreeBSDVersion  string    `json:"freebsd_version,omitempty"`
	VersionsDiffer  bool      `json:"versions_differ"`
	BuildSeconds    *int64    `json:"build_seconds,omitempty"`
	LastSeen        int64     `json:"last_seen,omitempty"`
	Size            *int64    `json:"size,omitempty"`
	Consumers       *int      `json:"consumers,omitempty"`
	Tracked         bool      `json:"tracked"`
	Compiled        bool      `json:"compiled"`
	WorkerFetched   bool      `json:"worker_fetched,omitempty"`
	HistoryOnly     bool      `json:"history_only,omitempty"`
	PublicationNote string    `json:"publication_note,omitempty"`
	BuildABI        string    `json:"build_abi,omitempty"`
	Cached          bool      `json:"cached"`
	CacheVersion    string    `json:"cache_version,omitempty"`
	CacheKey        string    `json:"cache_key,omitempty"`
	builtAt         time.Time // Matching retained build, including one with unknown duration.
}

// A bulk run can record successful builds well before Poudriere publishes
// its repository. Keep their records visible alongside the available archives.
func ManagementPackages(s *Snapshot, tracked []database.TrackedPackageRecord, origins []string, cache []cachedArchive, ports map[string]builder.ResolvedPort, builds []database.BuildRecord) PackageInventoryView {
	view := PackageInventoryView{Packages: []managementPackage{}, CachedCount: len(cache), CatalogReady: s != nil, OriginsReady: origins != nil}
	targets := make(map[string]bool)
	for _, origin := range origins {
		targets[origin] = true
	}
	activity := make(map[string]database.TrackedPackageRecord, len(tracked))
	wanted := make(map[string]packageinfo.Package)
	for _, t := range tracked {
		spec := t.BuildOrigin
		if spec == "" {
			spec = t.Origin
		}
		targets[spec] = true
		activity[t.Name] = t
		wanted[t.Name] = packageinfo.Package{Name: t.Name, Origin: t.Origin, BuildOrigin: spec}
	}
	view.TrackedOrigins = len(targets)
	for name, port := range ports {
		if targets[port.Origin] {
			wanted[name] = packageinfo.Package{Name: name, Origin: packageinfo.BasePort(port.Origin), BuildOrigin: port.Origin}
		}
	}
	if s != nil {
		for _, p := range s.ByPath {
			if targets[p.BuildOrigin] {
				wanted[p.Name] = p
			}
		}
	}

	seenNames, seenOrigins := make(map[string]bool), make(map[string]bool)
	rowFor := func(p packageinfo.Package) managementPackage {
		spec := p.BuildOrigin
		if spec == "" {
			spec = p.Origin
		}
		t, used := activity[p.Name]
		row := managementPackage{Name: p.Name, Origin: p.Origin, BuildOrigin: spec, Tracked: targets[spec] || used, LastSeen: t.LastSeen}
		if p.Name != "" {
			row.Consumers = &t.Consumers
			seenNames[p.Name] = true
		}
		seenOrigins[spec] = true
		if port, ok := ports[p.Name]; ok && packageinfo.BasePort(port.Origin) == p.Origin {
			row.PortVersion = port.Version
		}
		if s != nil {
			if upstream, ok := s.FreeBSDPackage(p.Name); ok {
				row.FreeBSDVersion = upstream.Version
			}
			row.LocalVersion = s.Compiled[p.Name].Version
			if current, ok := s.ByPath[s.PathByName[p.Name]]; ok {
				row.Source, row.Version, row.Size = "freebsd", current.Version, &current.Size
				if current.Local && !s.Fetched[p.Name] {
					row.Source = "poudrine"
				}
			}
		}
		return row
	}

	compiledRows := make(map[string]int)
	workerFetchedRows := make(map[string]int)
	if s != nil {
		var compiledBytes int64
		for name, local := range s.Compiled {
			compiledBytes += local.Size
			row := rowFor(local)
			row.Source, row.Version, row.Size, row.Compiled = "poudrine", local.Version, &local.Size, true
			row.VersionsDiffer = local.Version != "" && row.FreeBSDVersion != "" && ComparePkgVersions(local.Version, row.FreeBSDVersion) != 0
			row.LastSeen = max(row.LastSeen, s.compiledAt[name].Unix())
			compiledRows[name] = len(view.Packages)
			view.Packages = append(view.Packages, row)
		}
		view.CompiledBytes = &compiledBytes
		for name, fetched := range s.Fetched {
			p := s.ByPath[s.PathByName[name]]
			if !fetched || !p.Local {
				continue
			}
			row := rowFor(p)
			row.WorkerFetched = true
			workerFetchedRows[name] = len(view.Packages)
			view.Packages = append(view.Packages, row)
		}
	}
	for _, entry := range cache {
		p := entry.Package
		view.CachedBytes += p.Size
		if i, ok := workerFetchedRows[p.Name]; ok {
			row := &view.Packages[i]
			selected := s.ByPath[s.PathByName[p.Name]]
			if p.Version == selected.Version && p.Size == selected.Size && !row.Cached && matchesBuildIdentity(selected, cmp.Or(p.BuildOrigin, p.Origin), p.ABI) {
				row.Cached, row.CacheVersion, row.CacheKey = true, p.Version, entry.Key
				row.LastSeen = max(row.LastSeen, entry.Used.Unix())
				continue
			}
		}
		row := rowFor(p)
		row.Source, row.Version, row.Size, row.Cached = "freebsd", p.Version, &p.Size, true
		row.CacheVersion, row.CacheKey, row.LastSeen = p.Version, entry.Key, entry.Used.Unix()
		view.Packages = append(view.Packages, row)
	}
	for _, build := range builds {
		name, version := packageinfo.SplitBuildPackage(build.Package)
		if name == "" || build.Outcome != "succeeded" {
			continue
		}
		var local, upstream packageinfo.Package
		if s != nil {
			local = s.Compiled[name]
			upstream, _ = s.FreeBSDPackage(name)
		}
		localMatch := matchesBuildIdentity(local, build.Origin, build.ABI)
		if i, ok := compiledRows[name]; ok && localMatch && local.Version == version {
			row := &view.Packages[i]
			if build.Finished.After(row.builtAt) || build.Finished.Equal(row.builtAt) && row.BuildSeconds == nil {
				row.builtAt, row.BuildSeconds = build.Finished, build.BuildSeconds
				row.LastSeen = max(row.LastSeen, build.Finished.Unix())
			}
			continue
		}
		// Older history records have only a package ID. Resolve their display
		// origin when it is known from the active ports tree or repository.
		origin := build.Origin
		if origin == "" {
			origin = cmp.Or(ports[name].Origin, wanted[name].BuildOrigin, local.BuildOrigin, upstream.BuildOrigin, local.Origin, upstream.Origin)
		}
		row := rowFor(packageinfo.Package{Name: name, Origin: packageinfo.BasePort(origin), BuildOrigin: origin})
		row.HistoryOnly, row.BuildABI, row.BuildSeconds = true, build.ABI, build.BuildSeconds
		if s != nil {
			if hold, ok := s.LiveHolds[build.Package]; ok && matchesBuildIdentity(hold.pkg, build.Origin, build.ABI) {
				row.PublicationNote = hold.Reason
			}
		}
		row.Source, row.Version, row.Size = "poudrine", version, nil
		row.LastSeen = build.Finished.Unix()
		if !localMatch {
			row.LocalVersion = ""
		}
		if !matchesBuildIdentity(upstream, build.Origin, build.ABI) {
			row.FreeBSDVersion = ""
		}
		view.Packages = append(view.Packages, row)
	}
	for name, p := range wanted {
		if !seenNames[name] {
			view.Packages = append(view.Packages, rowFor(p))
		}
	}
	for origin := range targets {
		if !seenOrigins[origin] {
			view.Packages = append(view.Packages, rowFor(packageinfo.Package{Origin: packageinfo.BasePort(origin), BuildOrigin: origin}))
		}
	}
	slices.SortFunc(view.Packages, func(a, b managementPackage) int {
		return cmp.Or(cmp.Compare(a.BuildOrigin, b.BuildOrigin), cmp.Compare(a.Name, b.Name), cmp.Compare(b.Source, a.Source),
			cmp.Compare(a.Version, b.Version), cmp.Compare(a.BuildABI, b.BuildABI), cmp.Compare(a.CacheKey, b.CacheKey))
	})
	return view
}

func matchesBuildIdentity(p packageinfo.Package, origin, abi string) bool {
	return (origin == "" || origin == p.BuildOrigin || origin == p.Origin) &&
		(abi == "" || p.ABI == "" || packageinfo.CompatibleABI(packageinfo.PackageABI(abi, ""), p.ABI))
}

// Match the most recent successful build of the exact published package
// version. Retained failures, fetched archives and similarly named packages
// from a different origin or jail cannot supply its duration.
func LoadBuildTimes(ctx context.Context, s database.DB, snapshot *Snapshot, packages []managementPackage) error {
	pending := make(map[string]*managementPackage)
	var names []string
	for i := range packages {
		row := &packages[i]
		if row.Compiled && row.builtAt.IsZero() {
			pkg := row.Name + "-" + row.Version
			names = append(names, pkg)
			pending[pkg] = row
		}
	}
	if len(names) == 0 {
		return nil
	}
	timings, err := s.CompiledPackageTimes(ctx, names)
	if err != nil {
		return err
	}
	for _, timing := range timings {
		pkg, origin, abi, seconds := timing.Package, timing.Origin, timing.ABI, timing.Seconds
		row := pending[pkg]
		if row == nil || !matchesBuildIdentity(snapshot.Compiled[row.Name], origin, abi) {
			continue
		}
		row.BuildSeconds = seconds
		delete(pending, pkg)
		if len(pending) == 0 {
			break
		}
	}
	return nil
}

// The scanner runs independently of catalog signing. Keep its last errors for
// Management even when a read or signing failure prevented a snapshot swap.
type liveScanReport struct {
	failures map[string]liveCaptureFailure
	err      string
}

func (c *Catalog) recordLiveScan(failures map[string]liveCaptureFailure, scanErr error) {
	if scanErr == nil && len(failures) == 0 {
		c.LiveScan.Store(nil)
		return
	}
	report := &liveScanReport{failures: maps.Clone(failures)}
	if scanErr != nil {
		report.err = scanErr.Error()
	}
	c.LiveScan.Store(report)
}

func DescribeUnpublished(rows []managementPackage, s *Snapshot, scan *liveScanReport) {
	for i := range rows {
		row := &rows[i]
		if !row.HistoryOnly {
			continue
		}
		if scan != nil {
			if failure, ok := scan.failures[row.Name+"-"+row.Version]; ok && (row.BuildOrigin == failure.origin || row.Origin == failure.origin) {
				row.PublicationNote = "Could not stage the finished archive (" + failure.message + ")."
			} else if row.PublicationNote == "" && scan.err != "" {
				row.PublicationNote = "The latest publication check failed (" + scan.err + ")."
			}
		}
		if row.PublicationNote != "" {
			continue
		}
		if s == nil {
			row.PublicationNote = "Waiting for the first verified repository catalog."
			continue
		}
		row.PublicationNote = "No matching compiled archive in the current catalog."
		if p, ok := s.ByPath[s.PathByName[row.Name]]; ok && matchesBuildIdentity(p, row.BuildOrigin, row.BuildABI) {
			row.PublicationNote += " pkg currently offers " + p.Version + "."
		}
	}
}
