package catalog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/fsutil"
	"github.com/inkeliz/poudrine/internal/packageinfo"
)

type manifest struct {
	Pkg      packageinfo.Package `json:"-"`
	Raw      json.RawMessage     `json:"-"`
	provides []string
}

type packageAnnotations struct {
	Flavor, Subpackage string
	OverlayDigest      string `json:"poudrine_overlay"`
}

type repoContent struct {
	ByName map[string]manifest `json:"-"`
	extras map[string]json.RawMessage
}

type MetadataFile struct {
	Body []byte `json:"-"`
	etag string
}

type Snapshot struct {
	Overlays      config.PackageOverlays `json:"-"`
	bootstrapPath string
	ByPath        map[string]packageinfo.Package `json:"-"`
	PathByName    map[string]string              `json:"-"`
	// Keep FreeBSD versions for comparison even when local policy hides them.
	Displaced     map[string]packageinfo.Package `json:"-"`
	Compiled      map[string]packageinfo.Package `json:"-"`
	compiledAt    map[string]time.Time
	compiledFiles map[string]*CompiledArchive
	LiveHolds     map[string]liveHold     `json:"-"`
	Fetched       map[string]bool         `json:"-"`
	beforeLive    map[string]string       // baseline versions displaced by early publication
	Metadata      map[string]MetadataFile `json:"-"`
	updated       time.Time
	Ahead         int `json:"-"`
}

func (s *Snapshot) offeredVersion(name string) string {
	if version, staged := s.beforeLive[name]; staged {
		return version
	}
	return s.ByPath[s.PathByName[name]].Version
}

func (s *Snapshot) FreeBSDPackage(name string) (packageinfo.Package, bool) {
	if s != nil {
		if p, ok := s.Displaced[name]; ok {
			return p, true
		}
		if p, ok := s.ByPath[s.PathByName[name]]; ok && !p.Local {
			return p, true
		}
	}
	return packageinfo.Package{}, false
}

// Local packages carry the settings fingerprint baked into their manifest by
// the ports framework. An older build can have precisely the same pkg version.
func (s *Snapshot) PermitsLocal(p packageinfo.Package, fetched bool) bool {
	if s == nil || len(s.Overlays) == 0 {
		return p.OverlayDigest == ""
	}
	origin := packageinfo.BasePort(p.Origin)
	// A different port that happens to emit the same package name cannot
	// supply an overlaid FreeBSD package either.
	if stock, ok := s.Displaced[p.Name]; ok {
		stockOrigin := packageinfo.BasePort(stock.Origin)
		if _, reserved := s.Overlays[stockOrigin]; reserved && (stockOrigin != origin || !packageinfo.CompatibleABI(p.ABI, stock.ABI)) {
			return false
		}
	}
	overlay, configured := s.Overlays[origin]
	if !configured {
		return p.OverlayDigest == ""
	}
	return !fetched && p.OverlayDigest != "" && p.OverlayDigest == overlay.Digest()
}

type Catalog struct {
	Upstream               *url.URL               `json:"-"`
	Overlays               config.PackageOverlays `json:"-"`
	fingerprints, localDir string
	JailRoot               string          `json:"-"`
	Client                 *http.Client    `json:"-"`
	Key                    *rsa.PrivateKey `json:"-"`
	PublicKey              []byte
	refresh                sync.Mutex
	publish                sync.Mutex // serializes snapshot changes, independent of upstream HTTP requests
	revision               CatalogRevision
	upstreamContent        *repoContent           // immutable, authenticated input for local-only publications
	LiveDir                string                 `json:"-"`
	Live                   map[string]LivePackage `json:"-"`
	localIndex             os.FileInfo
	localArchives          map[string]os.FileInfo
	baseLibraries          map[string]bool
	Current                atomic.Pointer[Snapshot]       `json:"-"`
	LiveScan               atomic.Pointer[liveScanReport] `json:"-"`
}

func New(stateDir, upstreamURL, fingerprintsDir, localRepoDir string, client *http.Client) (*Catalog, error) {
	u, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("invalid upstream repository URL")
	}
	key, err := catalogKey(stateDir)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	publicKey := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if err := fsutil.WriteAtomicFile(filepath.Join(stateDir, "repository.pub"), publicKey, 0644); err != nil {
		return nil, fmt.Errorf("write repository public key: %w", err)
	}
	client = upstreamClient(client)
	return &Catalog{Upstream: u, fingerprints: fingerprintsDir, localDir: localRepoDir, LiveDir: filepath.Join(stateDir, config.LiveArchiveDirName), Client: client, Key: key, PublicKey: publicKey}, nil
}

func (c *Catalog) Package(p string) (packageinfo.Package, bool) {
	if s := c.Current.Load(); s != nil {
		if p == "Latest/pkg.pkg" || p == "Latest/pkg.txz" || BootstrapSignaturePath(p) {
			p = s.bootstrapPath
		}
		pkg, ok := s.ByPath[p]
		return pkg, ok
	}
	return packageinfo.Package{}, false
}

type CatalogStats struct {
	Packages int        `json:"packages"`
	Ahead    int        `json:"ahead"`
	Updated  *time.Time `json:"catalog_updated"`
}

func (c *Catalog) Stats() CatalogStats {
	if s := c.Current.Load(); s != nil {
		return CatalogStats{Packages: len(s.ByPath), Ahead: s.Ahead, Updated: &s.updated}
	}
	return CatalogStats{}
}

func (c *Catalog) Metadata(w http.ResponseWriter, r *http.Request, p string) bool {
	switch p {
	case "meta.conf":
	case "packagesite.pkg", "packagesite.txz", "packagesite.tzst":
		p = "packagesite.pkg"
	case "data.pkg", "data.txz", "data.tzst":
		p = "data.pkg"
	case "meta.pkg", "meta.txz", "meta.tzst":
		p = "meta.pkg"
	default:
		return false
	}
	s := c.Current.Load()
	if s == nil {
		http.Error(w, "repository catalog is initializing", http.StatusServiceUnavailable)
		return true
	}
	f := s.Metadata[p]
	w.Header().Set("ETag", f.etag)
	if p == "meta.conf" {
		w.Header().Set("Content-Type", "text/plain")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeContent(w, r, p, s.updated, bytes.NewReader(f.Body))
	return true
}

func ParseCatalog(b []byte, member string, local bool) (repoContent, error) {
	repo := repoContent{ByName: make(map[string]manifest), extras: map[string]json.RawMessage{"groups": json.RawMessage("[]"), "expired_packages": json.RawMessage("[]")}}
	add := func(raw json.RawMessage) error {
		var m struct {
			Path, Repopath, Name, Origin, Version, Sum, ABI, Arch string
			Pkgsize                                               *int64
			Provides                                              []string `json:"shlibs_provided"`
			Annotations                                           packageAnnotations
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		if m.Path == "" {
			m.Path = m.Repopath
		}
		if m.Path == "" || !filepath.IsLocal(m.Path) || filepath.Clean(m.Path) != m.Path || m.Path == "." ||
			strings.ContainsAny(m.Path, "\\?#\x00\r\n") || m.Repopath != "" && m.Repopath != m.Path {
			return fmt.Errorf("invalid package archive path %q", m.Path)
		}
		if m.Name == "" || m.Origin == "" || m.Version == "" || m.Pkgsize == nil || *m.Pkgsize < 0 {
			return fmt.Errorf("incomplete manifest for %q", m.Name)
		}
		if _, err := packageHasher(m.Sum); err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}
		origin := packageinfo.PackageBuildOrigin(m.Origin, m.Annotations.Flavor, m.Annotations.Subpackage)
		p := packageinfo.Package{Path: m.Path, Name: m.Name, Version: m.Version, Origin: m.Origin, BuildOrigin: origin, Sum: m.Sum, Size: *m.Pkgsize, Local: local, ABI: packageinfo.PackageABI(m.ABI, m.Arch)}
		p.OverlayDigest = m.Annotations.OverlayDigest
		if old, ok := repo.ByName[p.Name]; ok && ComparePkgVersions(old.Pkg.Version, p.Version) >= 0 {
			return nil
		}
		repo.ByName[p.Name] = manifest{Pkg: p, Raw: raw, provides: m.Provides}
		return nil
	}
	if member == "data" {
		var data map[string]json.RawMessage
		if err := json.Unmarshal(b, &data); err != nil {
			return repo, err
		}
		var packages []json.RawMessage
		if err := json.Unmarshal(data["packages"], &packages); err != nil || packages == nil {
			return repo, errors.New("data catalog missing packages array")
		}
		for _, raw := range packages {
			if err := add(raw); err != nil {
				return repo, err
			}
		}
		delete(data, "packages")
		maps.Copy(repo.extras, data)
	} else {
		scanner := bufio.NewScanner(bytes.NewReader(b))
		scanner.Buffer(make([]byte, 64<<10), 32<<20)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			if err := add(bytes.Clone(line)); err != nil {
				return repo, err
			}
		}
		if err := scanner.Err(); err != nil {
			return repo, err
		}
	}
	return repo, nil
}

const RepoMeta = "version = 2;\npacking_format = \"tzst\";\nmanifests = \"packagesite.yaml\";\nmanifests_archive = \"packagesite\";\ndata = \"data\";\n"

func (c *Catalog) Refresh(ctx context.Context) error {
	c.refresh.Lock()
	defer c.refresh.Unlock()
	check, info, err := c.OpenLocalIndex()
	if err != nil {
		return fmt.Errorf("open local Poudriere catalog: %w", err)
	}
	if check != nil {
		check.Close()
	}
	var previous CatalogRevision
	c.publish.Lock()
	if c.revision.archive != "" && c.localInputsUnchanged(info) {
		previous = c.revision
	}
	c.publish.Unlock()
	repo, revision, err := c.fetchCatalog(ctx, previous)
	if err != nil {
		return err
	}
	c.publish.Lock()
	defer c.publish.Unlock()
	// A bulk may have committed or published more packages during the fetch.
	index, info, err := c.OpenLocalIndex()
	if err != nil {
		return fmt.Errorf("open local Poudriere catalog: %w", err)
	}
	if index != nil {
		defer index.Close()
	}
	if repo == nil {
		if c.localInputsUnchanged(info) {
			c.revision = revision
			return nil
		}
		repo = c.upstreamContent
	}
	return c.PublishCatalog(ctx, repo, revision, index, info, c.Live)
}

// publishCatalog builds both signed indexes before making any input or output
// visible. The caller holds publish. A failed rebuild leaves the last good
// upstream, local packages and signed snapshot available for retry.
func (c *Catalog) PublishCatalog(ctx context.Context, upstream *repoContent, revision CatalogRevision, index *os.File, info os.FileInfo, live map[string]LivePackage) error {
	repo := repoContent{ByName: maps.Clone(upstream.ByName), extras: upstream.extras}
	var local repoContent
	if index != nil {
		member := "packagesite.yaml"
		if strings.HasPrefix(filepath.Base(index.Name()), "data.") {
			member = "data"
		}
		m, err := UnpackCatalog(index, member)
		if err != nil {
			return fmt.Errorf("local Poudriere catalog: %w", err)
		}
		local, err = ParseCatalog(m[member], member, true)
		if err != nil {
			return fmt.Errorf("local Poudriere catalog: %w", err)
		}
	}
	localFiles := make(map[string]os.FileInfo, len(local.ByName))
	if len(local.ByName) != 0 {
		localFiles[".pkg-cache"], _ = os.Stat(filepath.Join(c.localDir, ".pkg-cache"))
	}
	s := &Snapshot{
		Overlays:      c.Overlays,
		ByPath:        make(map[string]packageinfo.Package, len(repo.ByName)),
		PathByName:    make(map[string]string, len(repo.ByName)),
		Displaced:     make(map[string]packageinfo.Package),
		Compiled:      make(map[string]packageinfo.Package, len(local.ByName)),
		compiledAt:    make(map[string]time.Time, len(local.ByName)),
		compiledFiles: make(map[string]*CompiledArchive, len(local.ByName)),
		Fetched:       make(map[string]bool),
		Metadata:      make(map[string]MetadataFile),
	}
	arches := map[string]bool{}
	for name, m := range repo.ByName {
		if i := strings.LastIndexByte(m.Pkg.ABI, ':'); i > 0 {
			arches[m.Pkg.ABI] = true
			arches[m.Pkg.ABI[:i]+":*"] = true
		}
		if _, overlaid := c.Overlays[packageinfo.BasePort(m.Pkg.Origin)]; overlaid {
			s.Displaced[name] = m.Pkg
			delete(repo.ByName, name)
		}
	}
	for name, m := range local.ByName {
		if !arches[m.Pkg.ABI] {
			continue
		}
		if !s.PermitsLocal(m.Pkg, false) {
			continue
		}
		old, exists := repo.ByName[name]
		if exists && !packageinfo.CompatibleABI(m.Pkg.ABI, old.Pkg.ABI) {
			continue
		}
		versionOrder := ComparePkgVersions(m.Pkg.Version, old.Pkg.Version)
		preferred := !exists || versionOrder > 0
		stat, err := os.Stat(filepath.Join(c.localDir, m.Pkg.Path))
		localFiles[m.Pkg.Path] = stat
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			if !preferred {
				continue
			}
			return err
		}
		if !stat.Mode().IsRegular() || stat.Size() != m.Pkg.Size {
			if !preferred {
				continue
			}
			return fmt.Errorf("local package %s differs from its catalog", m.Pkg.Path)
		}
		// Poudriere can satisfy build dependencies with prebuilt packages.
		// It hardlinks these from .pkg-cache into the local All directory.
		fetched, err := fetchedByPoudriere(c.localDir, m.Pkg.Path, stat)
		if err != nil {
			if preferred {
				return fmt.Errorf("identify native package %s: %w", m.Pkg.Path, err)
			}
			continue
		}
		if fetched && !s.PermitsLocal(m.Pkg, true) {
			continue
		}
		if !fetched {
			file := &CompiledArchive{Path: filepath.Join(c.localDir, m.Pkg.Path), Info: stat}
			if prev := c.Current.Load(); prev != nil && prev.Compiled[name] == m.Pkg {
				if prior := prev.compiledFiles[name]; prior != nil && prior.Path == file.Path && sameArchive(prior.Info, stat) {
					file = prior
				}
			}
			// Validate custom builds and allow a verified native build to win
			// a version tie with the FreeBSD package.
			if m.Pkg.OverlayDigest != "" || exists && versionOrder == 0 && m.Pkg.BuildOrigin == old.Pkg.BuildOrigin {
				if err := file.verify(m.Pkg); err != nil {
					log.Printf("skipping invalid compiled package %s: %v", m.Pkg.Path, err)
					continue
				}
				preferred = true
			}
			s.Compiled[name] = m.Pkg
			s.compiledAt[name] = stat.ModTime()
			s.compiledFiles[name] = file
		}
		if !preferred {
			continue
		}
		if fetched {
			s.Fetched[name] = true
		}
		repo.ByName[name] = m
		if exists {
			s.Displaced[name] = old.Pkg
		}
	}
	var baseLibraries map[string]bool
	if len(live) > 0 {
		baseLibraries = jailBaseLibraries(c.JailRoot)
		mergeLivePackages(live, repo.ByName, arches, baseLibraries, s)
	}
	// Emit the same complete manifests in both pkg formats, including fields
	// introduced after this software was built.
	names := slices.Sorted(maps.Keys(repo.ByName))
	var lines, data bytes.Buffer
	extras, err := json.Marshal(repo.extras)
	if err != nil {
		return err
	}
	data.Write(extras[:len(extras)-1])
	data.WriteString(",\"packages\":[")
	for i, k := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		m := repo.ByName[k]
		if _, exists := s.ByPath[m.Pkg.Path]; exists {
			return fmt.Errorf("duplicate catalog archive path %q", m.Pkg.Path)
		}
		s.ByPath[m.Pkg.Path] = m.Pkg
		s.PathByName[m.Pkg.Name] = m.Pkg.Path
		if original, ok := s.Displaced[k]; ok && !s.Fetched[k] && ComparePkgVersions(m.Pkg.Version, original.Version) > 0 {
			s.Ahead++
		}
		if m.Pkg.Name == "pkg" {
			s.bootstrapPath = m.Pkg.Path
		}
		// Input packagesite rows are single-line; data catalogs may be indented.
		var compact bytes.Buffer
		if bytes.ContainsAny(m.Raw, "\r\n") {
			if err := json.Compact(&compact, m.Raw); err != nil {
				return err
			}
			m.Raw = compact.Bytes()
		}
		lines.Write(m.Raw)
		lines.WriteByte('\n')
		if i != 0 {
			data.WriteByte(',')
		}
		data.Write(m.Raw)
	}
	data.WriteString("]}")
	addFile := func(name string, b []byte) {
		s.Metadata[name] = MetadataFile{b, fmt.Sprintf("\"%x\"", sha256.Sum256(b))}
	}
	addFile("meta.conf", []byte(RepoMeta))
	for _, f := range []struct {
		archive, member string
		body            []byte
	}{{"meta.pkg", "meta", []byte(RepoMeta)}, {"packagesite.pkg", "packagesite.yaml", lines.Bytes()}, {"data.pkg", "data", data.Bytes()}} {
		archive, err := SignedPkg(c.Key, f.member, f.body)
		if err != nil {
			return err
		}
		addFile(f.archive, archive)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.updated = time.Now().UTC()
	if prev := c.Current.Load(); prev != nil {
		if s.Metadata["data.pkg"].etag == prev.Metadata["data.pkg"].etag {
			s.updated = prev.updated
		} else if second := s.updated.Truncate(time.Second); second.Equal(prev.updated.Truncate(time.Second)) {
			// pkg revalidates by whole-second Last-Modified. A local build
			// and a regular refresh can both publish within the same second.
			// Wait for the next timestamp so clients see the second update.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Until(second.Add(time.Second))):
				s.updated = time.Now().UTC()
			}
		}
	}
	if err := checkUpstreamFingerprint(c.fingerprints, revision.Fingerprint); err != nil {
		return err
	}
	if err := c.retainLiveDownloads(s, live); err != nil {
		return err
	}
	c.revision, c.upstreamContent, c.localIndex, c.localArchives = revision, upstream, info, localFiles
	c.Live = maps.Clone(live)
	c.baseLibraries = baseLibraries
	c.Current.Store(s)
	return nil
}

// A compiled archive can replace several cached versions. Hash its bytes
// once, and reuse the result while the file and declared checksum are unchanged.
type CompiledArchive struct {
	Path     string      `json:"-"`
	Info     os.FileInfo `json:"-"`
	mu       sync.Mutex
	verified bool
}

func sameArchive(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}

func fetchedByPoudriere(dir, path string, archive os.FileInfo) (bool, error) {
	base := filepath.Base(path)
	names := []string{base}
	// pkg repo may move the hardlink into All/Hashed/name-version~digest.pkg;
	// Poudriere originally linked it from .pkg-cache/name-version.pkg.
	if filepath.Base(filepath.Dir(path)) == "Hashed" {
		if split := strings.LastIndexByte(base, '~'); split > 0 {
			names = append(names, base[:split]+filepath.Ext(base))
		}
	}
	for _, name := range names {
		info, err := os.Stat(filepath.Join(dir, ".pkg-cache", name))
		if err == nil && os.SameFile(archive, info) {
			return true, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (a *CompiledArchive) verify(p packageinfo.Package) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !sameArchive(a.Info, info) {
		return errUntrustedPackage
	}
	if !a.verified {
		if p.OverlayDigest != "" {
			// A native repository index can outlive or mislabel the archive.
			// Check the provenance that was written into the package itself.
			raw, err := liveCompactJSON(f)
			if err != nil {
				return err
			}
			var built struct {
				Name, Origin, Version string
				Annotations           packageAnnotations
			}
			if err := json.Unmarshal(raw, &built); err != nil {
				return err
			}
			if built.Annotations.OverlayDigest != p.OverlayDigest || built.Name != p.Name || built.Version != p.Version ||
				packageinfo.PackageBuildOrigin(built.Origin, built.Annotations.Flavor, built.Annotations.Subpackage) != p.BuildOrigin {
				return errUntrustedPackage
			}
		}
		if err := VerifyPackage(f, io.Discard, p); err != nil {
			return err
		}
		a.verified = true
	}
	return nil
}

// HTTP validators belong to one verified upstream archive. The signed digest
// also identifies an unchanged catalog on mirrors that always return HTTP 200.
type CatalogRevision struct {
	archive, resource, etag, modified string
	Fingerprint, signature            string `json:"-"`
}

func (c *Catalog) OpenLocalIndex() (*os.File, os.FileInfo, error) {
	if c.localDir != "" {
		for _, name := range []string{"data.pkg", "data.tzst", "data.txz", "packagesite.pkg", "packagesite.txz", "packagesite.tzst"} {
			f, err := os.Open(filepath.Join(c.localDir, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, nil, err
			}
			return f, info, nil
		}
	}
	return nil, nil, nil
}

func sameLocalIndex(old, index os.FileInfo) bool {
	if index == nil || old == nil {
		return index == nil && old == nil
	}
	return sameArchive(old, index)
}

// A native commit is a local publication event even when there were no
// captured packages, or the next upstream HTTP request has not completed.
func (c *Catalog) PublicationInputsChanged() bool {
	f, info, err := c.OpenLocalIndex()
	if err != nil {
		return true // PublishLive reports the read error to Management.
	}
	if f != nil {
		f.Close()
	}
	c.publish.Lock()
	defer c.publish.Unlock()
	return !c.samePublicationInputs(info)
}

func (c *Catalog) samePublicationInputs(index os.FileInfo) bool {
	return sameLocalIndex(c.localIndex, index) &&
		(len(c.Live) == 0 || maps.Equal(c.baseLibraries, jailBaseLibraries(c.JailRoot)))
}

func (c *Catalog) localInputsUnchanged(index os.FileInfo) bool {
	if !c.samePublicationInputs(index) {
		return false
	}
	// Even with the same index, a previously missing local archive might
	// have appeared, or an archive selected for publication disappeared.
	for path, old := range c.localArchives {
		fi, err := os.Stat(filepath.Join(c.localDir, path))
		if old == nil && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || old == nil || !sameArchive(fi, old) {
			return false
		}
	}
	return true
}

// A nil result means the existing merged snapshot still applies. Only send
// validators if both that snapshot's local inputs and its signing trust hold.
func (c *Catalog) fetchCatalog(ctx context.Context, previous CatalogRevision) (*repoContent, CatalogRevision, error) {
	for _, spec := range []struct{ archive, member string }{{"data.pkg", "data"}, {"packagesite.pkg", "packagesite.yaml"}, {"packagesite.txz", "packagesite.yaml"}, {"packagesite.tzst", "packagesite.yaml"}} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Upstream.JoinPath(spec.archive).String(), nil)
		if err != nil {
			return nil, CatalogRevision{}, err
		}
		req.Header.Set("Cache-Control", "no-cache")
		conditional := previous.archive == spec.archive && (previous.etag != "" || previous.modified != "") &&
			checkUpstreamFingerprint(c.fingerprints, previous.Fingerprint) == nil
		if conditional {
			if previous.etag != "" {
				req.Header.Set("If-None-Match", previous.etag)
			}
			if previous.modified != "" {
				req.Header.Set("If-Modified-Since", previous.modified)
			}
		}
		resp, err := c.Client.Do(req)
		if err != nil {
			return nil, CatalogRevision{}, err
		}
		// A mirror redirect can change between polls. A different resource
		// may use the same opaque ETag or modification date for other data.
		if conditional && resp.StatusCode == http.StatusNotModified && resp.Request.URL.String() != previous.resource {
			resp.Body.Close()
			req.Header.Del("If-None-Match")
			req.Header.Del("If-Modified-Since")
			conditional = false
			resp, err = c.Client.Do(req)
			if err != nil {
				return nil, CatalogRevision{}, err
			}
		}
		switch resp.StatusCode {
		case http.StatusNotFound:
			resp.Body.Close()
			continue
		case http.StatusNotModified:
			resp.Body.Close()
			if !conditional {
				return nil, CatalogRevision{}, fmt.Errorf("upstream %s: unsolicited HTTP 304", spec.archive)
			}
			// Trust can change while the request is in flight.
			if err := checkUpstreamFingerprint(c.fingerprints, previous.Fingerprint); err != nil {
				return nil, CatalogRevision{}, err
			}
			return nil, previous, nil
		case http.StatusOK:
		default:
			resp.Body.Close()
			return nil, CatalogRevision{}, fmt.Errorf("upstream %s: HTTP %d", spec.archive, resp.StatusCode)
		}

		m, err := UnpackCatalog(io.LimitReader(resp.Body, maxCatalogBytes), spec.member)
		resp.Body.Close()
		if err != nil {
			return nil, CatalogRevision{}, err
		}
		if err = verifyCatalog(m, spec.member, c.fingerprints); err != nil {
			return nil, CatalogRevision{}, fmt.Errorf("authenticate %s: %w", spec.archive, err)
		}
		revision := CatalogRevision{
			archive: spec.archive, resource: resp.Request.URL.String(), etag: resp.Header.Get("ETag"),
			Fingerprint: fmt.Sprintf("%x", sha256.Sum256(m[spec.member+".pub"])),
			signature:   string(m[spec.member+".sig"]),
		}
		// "*" is a request wildcard, never an entity tag for this catalog.
		if revision.etag == "*" {
			revision.etag = ""
		}
		// Last-Modified has one-second resolution. A fresh upload can still
		// change within that second. Wait for an older response before using
		// the date as a validator (RFC 9110 section 8.8.2.2).
		date, dateErr := http.ParseTime(resp.Header.Get("Date"))
		modified, modifiedErr := http.ParseTime(resp.Header.Get("Last-Modified"))
		if dateErr == nil && modifiedErr == nil && date.Sub(modified) >= time.Minute {
			revision.modified = resp.Header.Get("Last-Modified")
		}
		if previous.archive == revision.archive && previous.Fingerprint == revision.Fingerprint && previous.signature == revision.signature {
			return nil, revision, nil
		}
		repo, err := ParseCatalog(m[spec.member], spec.member, false)
		return &repo, revision, err
	}
	return nil, CatalogRevision{}, errors.New("no upstream catalog")
}
