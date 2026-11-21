package poudrine

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const packagesPath = "/api/admin/packages"

type packageInventoryView struct {
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
	Name           string    `json:"name"`
	Origin         string    `json:"origin"`
	BuildOrigin    string    `json:"build_origin"`
	Source         string    `json:"source"`
	Version        string    `json:"version,omitempty"`
	PortVersion    string    `json:"port_version,omitempty"`
	LocalVersion   string    `json:"local_version,omitempty"`
	FreeBSDVersion string    `json:"freebsd_version,omitempty"`
	VersionsDiffer bool      `json:"versions_differ"`
	BuildSeconds   *int64    `json:"build_seconds,omitempty"`
	LastSeen       int64     `json:"last_seen,omitempty"`
	Size           *int64    `json:"size,omitempty"`
	Consumers      *int      `json:"consumers,omitempty"`
	Tracked        bool      `json:"tracked"`
	Compiled       bool      `json:"compiled"`
	HistoryOnly    bool      `json:"history_only,omitempty"`
	BuildABI       string    `json:"build_abi,omitempty"`
	Cached         bool      `json:"cached"`
	CacheVersion   string    `json:"cache_version,omitempty"`
	CacheKey       string    `json:"cache_key,omitempty"`
	builtAt        time.Time // Matching retained build, including one with unknown duration.
}

func (a *Admin) packageInventory(w http.ResponseWriter, r *http.Request, body []byte) {
	if a.cfg.Packages == nil {
		http.Error(w, "Package inventory unavailable", http.StatusServiceUnavailable)
		return
	}
	var origins []string
	var poudriereBytes *int64
	if a.cfg.broker == nil {
		origins = configuredOrigins(a.cfg.ActiveSettings.PackagesFile)
		if a.cfg.Build != nil {
			if status, err := a.cfg.Build(); err == nil {
				poudriereBytes = status.PoudriereBytes
			}
		}
	} else {
		// A temporarily unavailable worker should not hide the cache or client activity.
		reply, err := a.cfg.broker.exchange(forwardedConfigRequest(r, body))
		if err == nil && reply.Status == http.StatusOK {
			origins = reply.Origins
			poudriereBytes = reply.PoudriereBytes
		}
	}
	view, err := a.cfg.Packages(r.Context(), origins)
	if err != nil {
		adminInternal(w, err)
		return
	}
	view.PoudriereBytes = poudriereBytes
	adminJSON(w, view)
}

type resolvedPort struct {
	origin, version string
}

// Poudriere records the exact package name and version its ports tree resolved
// for this jail, including flavors, epochs and revisions, before starting jobs.
func resolvedPorts(cfg BuilderConfig) map[string]resolvedPort {
	root, err := os.OpenRoot(cfg.BaseFS)
	if err != nil {
		return nil
	}
	defer root.Close()
	log, err := root.OpenRoot(filepath.Join("data", "logs", "bulk", cfg.Jail+"-"+cfg.Ports, "latest"))
	if err != nil {
		return nil
	}
	defer log.Close()
	const maxBytes = 32 << 20
	data := progressValue(log, "all_pkgs%", maxBytes)
	pkgColumn, originColumn := 0, 1
	if data == "" {
		// Older or incomplete runs may only have the post-trim queue.
		data = progressValue(log, "ports.queued", maxBytes)
		pkgColumn, originColumn = 1, 0
	}
	versions := make(map[string]resolvedPort)
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pkg, origin := fields[pkgColumn], fields[originColumn]
		i := strings.LastIndexByte(pkg, '-')
		if i < 1 || i == len(pkg)-1 || len(pkg) > 512 || !jobPackage.MatchString(pkg) || !buildOrigin.MatchString(origin) {
			continue
		}
		versions[pkg[:i]] = resolvedPort{origin, pkg[i+1:]}
	}
	return versions
}

func basePort(origin string) string {
	if end := strings.IndexAny(origin, "@~"); end >= 0 {
		return origin[:end]
	}
	return origin
}

// A bulk run can record successful builds well before Poudriere publishes
// its repository. Keep their records visible alongside the available archives.
func managementPackages(s *catalogSnapshot, tracked []trackedPackageRecord, origins []string, cache []cachedArchive, ports map[string]resolvedPort, builds []BuildRecord) packageInventoryView {
	view := packageInventoryView{Packages: []managementPackage{}, CachedCount: len(cache), CatalogReady: s != nil, OriginsReady: origins != nil}
	targets := make(map[string]bool)
	for _, origin := range origins {
		targets[origin] = true
	}
	activity := make(map[string]trackedPackageRecord, len(tracked))
	wanted := make(map[string]CatalogPackage)
	for _, t := range tracked {
		spec := t.BuildOrigin
		if spec == "" {
			spec = t.Origin
		}
		targets[spec] = true
		activity[t.Name] = t
		wanted[t.Name] = CatalogPackage{Name: t.Name, Origin: t.Origin, BuildOrigin: spec}
	}
	view.TrackedOrigins = len(targets)
	for name, port := range ports {
		if targets[port.origin] {
			wanted[name] = CatalogPackage{Name: name, Origin: basePort(port.origin), BuildOrigin: port.origin}
		}
	}
	if s != nil {
		for _, p := range s.byPath {
			if targets[p.BuildOrigin] {
				wanted[p.Name] = p
			}
		}
	}

	seenNames, seenOrigins := make(map[string]bool), make(map[string]bool)
	rowFor := func(p CatalogPackage) managementPackage {
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
		if port, ok := ports[p.Name]; ok && basePort(port.origin) == p.Origin {
			row.PortVersion = port.version
		}
		if s != nil {
			if upstream, ok := s.freeBSDPackage(p.Name); ok {
				row.FreeBSDVersion = upstream.Version
			}
			row.LocalVersion = s.compiled[p.Name].Version
			if current, ok := s.byPath[s.pathByName[p.Name]]; ok {
				row.Source, row.Version, row.Size = "freebsd", current.Version, &current.Size
				if current.Local && !s.fetched[p.Name] {
					row.Source = "poudrine"
				}
			}
		}
		return row
	}

	compiledRows := make(map[string]int)
	if s != nil {
		var compiledBytes int64
		for name, local := range s.compiled {
			compiledBytes += local.Size
			row := rowFor(local)
			row.Source, row.Version, row.Size, row.Compiled = "poudrine", local.Version, &local.Size, true
			row.VersionsDiffer = local.Version != "" && row.FreeBSDVersion != "" && comparePkgVersions(local.Version, row.FreeBSDVersion) != 0
			row.LastSeen = max(row.LastSeen, s.compiledAt[name].Unix())
			compiledRows[name] = len(view.Packages)
			view.Packages = append(view.Packages, row)
		}
		view.CompiledBytes = &compiledBytes
	}
	for _, entry := range cache {
		p := entry.Package
		row := rowFor(p)
		row.Source, row.Version, row.Size, row.Cached = "freebsd", p.Version, &p.Size, true
		row.CacheVersion, row.CacheKey, row.LastSeen = p.Version, entry.Key, entry.Used.Unix()
		view.Packages = append(view.Packages, row)
		view.CachedBytes += p.Size
	}
	for _, build := range builds {
		name, version := splitBuildPackage(build.Package)
		if name == "" || build.Outcome != "succeeded" {
			continue
		}
		var local, upstream CatalogPackage
		if s != nil {
			local = s.compiled[name]
			upstream, _ = s.freeBSDPackage(name)
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
			origin = cmp.Or(ports[name].origin, wanted[name].BuildOrigin, local.BuildOrigin, upstream.BuildOrigin, local.Origin, upstream.Origin)
		}
		row := rowFor(CatalogPackage{Name: name, Origin: basePort(origin), BuildOrigin: origin})
		row.HistoryOnly, row.BuildABI, row.BuildSeconds = true, build.ABI, build.BuildSeconds
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
			view.Packages = append(view.Packages, rowFor(CatalogPackage{Origin: basePort(origin), BuildOrigin: origin}))
		}
	}
	slices.SortFunc(view.Packages, func(a, b managementPackage) int {
		return cmp.Or(cmp.Compare(a.BuildOrigin, b.BuildOrigin), cmp.Compare(a.Name, b.Name), cmp.Compare(b.Source, a.Source),
			cmp.Compare(a.Version, b.Version), cmp.Compare(a.BuildABI, b.BuildABI), cmp.Compare(a.CacheKey, b.CacheKey))
	})
	return view
}

func matchesBuildIdentity(p CatalogPackage, origin, abi string) bool {
	return (origin == "" || origin == p.BuildOrigin || origin == p.Origin) &&
		(abi == "" || p.ABI == "" || compatibleABI(packageABI(abi, ""), p.ABI))
}

// Match the most recent successful build of the exact published package
// version. Retained failures, fetched archives and similarly named packages
// from a different origin or jail cannot supply its duration.
func (s inventory) loadBuildTimes(ctx context.Context, snapshot *catalogSnapshot, packages []managementPackage) error {
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
	keys, err := json.Marshal(names)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT package,origin,abi,seconds FROM compiled_package_times
 WHERE package IN (SELECT value FROM json_each(?))
 ORDER BY package, started_ns DESC, seconds IS NOT NULL DESC`, string(keys))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pkg, origin, abi string
		var seconds *int64
		if err := rows.Scan(&pkg, &origin, &abi, &seconds); err != nil {
			return err
		}
		row := pending[pkg]
		if row == nil || !matchesBuildIdentity(snapshot.compiled[row.Name], origin, abi) {
			continue
		}
		row.BuildSeconds = seconds
		delete(pending, pkg)
		if len(pending) == 0 {
			break
		}
	}
	return rows.Err()
}
