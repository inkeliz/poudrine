package poudrine

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
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type CatalogPackage struct {
	Path, Name, Origin, BuildOrigin, Version, Sum, ABI string
	Size                                               int64
	Local                                              bool
	live                                               bool
	overlayDigest                                      string
}

type manifest struct {
	pkg      CatalogPackage
	raw      json.RawMessage
	provides []string
}
type packageAnnotations struct {
	Flavor, Subpackage string
	OverlayDigest      string `json:"poudrine_overlay"`
}
type repoContent struct {
	byName map[string]manifest
	extras map[string]json.RawMessage
}
type metadataFile struct {
	body []byte
	etag string
}
type catalogSnapshot struct {
	overlays      PackageOverlays
	bootstrapPath string
	byPath        map[string]CatalogPackage
	pathByName    map[string]string
	// Keep FreeBSD versions for comparison even when local policy hides them.
	displaced     map[string]CatalogPackage
	compiled      map[string]CatalogPackage
	compiledAt    map[string]time.Time
	compiledFiles map[string]*compiledArchive
	liveHolds     map[string]liveHold
	fetched       map[string]bool
	beforeLive    map[string]string // baseline versions displaced by early publication
	metadata      map[string]metadataFile
	updated       time.Time
	ahead         int
}

func (s *catalogSnapshot) offeredVersion(name string) string {
	if version, staged := s.beforeLive[name]; staged {
		return version
	}
	return s.byPath[s.pathByName[name]].Version
}

func (s *catalogSnapshot) freeBSDPackage(name string) (CatalogPackage, bool) {
	if s != nil {
		if p, ok := s.displaced[name]; ok {
			return p, true
		}
		if p, ok := s.byPath[s.pathByName[name]]; ok && !p.Local {
			return p, true
		}
	}
	return CatalogPackage{}, false
}

// Local packages carry the settings fingerprint baked into their manifest by
// the ports framework. An older build can have precisely the same pkg version.
func (s *catalogSnapshot) permitsLocal(p CatalogPackage, fetched bool) bool {
	if s == nil || len(s.overlays) == 0 {
		return p.overlayDigest == ""
	}
	origin := basePort(p.Origin)
	// A different port that happens to emit the same package name cannot
	// supply an overlaid FreeBSD package either.
	if stock, ok := s.displaced[p.Name]; ok {
		stockOrigin := basePort(stock.Origin)
		if _, reserved := s.overlays[stockOrigin]; reserved && (stockOrigin != origin || !compatibleABI(p.ABI, stock.ABI)) {
			return false
		}
	}
	overlay, configured := s.overlays[origin]
	if !configured {
		return p.overlayDigest == ""
	}
	return !fetched && p.overlayDigest != "" && p.overlayDigest == overlay.digest()
}

type Catalog struct {
	upstream               *url.URL
	overlays               PackageOverlays
	fingerprints, localDir string
	jailRoot               string
	client                 *http.Client
	key                    *rsa.PrivateKey
	PublicKey              []byte
	refresh                sync.Mutex
	publish                sync.Mutex // serializes snapshot changes, independent of upstream HTTP requests
	revision               catalogRevision
	upstreamContent        *repoContent // immutable, authenticated input for local-only publications
	liveDir                string
	live                   map[string]livePackage
	localIndex             os.FileInfo
	localArchives          map[string]os.FileInfo
	baseLibraries          map[string]bool
	current                atomic.Pointer[catalogSnapshot]
	liveScan               atomic.Pointer[liveScanReport]
}

func NewCatalog(stateDir, upstreamURL, fingerprintsDir, localRepoDir string, client *http.Client) (*Catalog, error) {
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
	if err := writeAtomicFile(filepath.Join(stateDir, "repository.pub"), publicKey, 0644); err != nil {
		return nil, fmt.Errorf("write repository public key: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Catalog{upstream: u, fingerprints: fingerprintsDir, localDir: localRepoDir, liveDir: filepath.Join(stateDir, liveArchiveDirName), client: client, key: key, PublicKey: publicKey}, nil
}

func (c *Catalog) Package(p string) (CatalogPackage, bool) {
	if s := c.current.Load(); s != nil {
		if p == "Latest/pkg.pkg" || p == "Latest/pkg.txz" || bootstrapSignaturePath(p) {
			p = s.bootstrapPath
		}
		pkg, ok := s.byPath[p]
		return pkg, ok
	}
	return CatalogPackage{}, false
}

type CatalogStats struct {
	Packages int        `json:"packages"`
	Ahead    int        `json:"ahead"`
	Updated  *time.Time `json:"catalog_updated"`
}

func (c *Catalog) Stats() CatalogStats {
	if s := c.current.Load(); s != nil {
		return CatalogStats{Packages: len(s.byPath), Ahead: s.ahead, Updated: &s.updated}
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
	s := c.current.Load()
	if s == nil {
		http.Error(w, "repository catalog is initializing", http.StatusServiceUnavailable)
		return true
	}
	f := s.metadata[p]
	w.Header().Set("ETag", f.etag)
	if p == "meta.conf" {
		w.Header().Set("Content-Type", "text/plain")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeContent(w, r, p, s.updated, bytes.NewReader(f.body))
	return true
}

func packageBuildOrigin(origin, flavor, subpackage string) string {
	if flavor != "" {
		origin += "@" + flavor
	}
	if subpackage != "" {
		origin += "~" + subpackage
	}
	return origin
}

func packageABI(abi, arch string) string {
	if abi == "" {
		abi = arch
	}
	abi = strings.ToLower(abi)
	abi = strings.ReplaceAll(abi, ":x86:64", ":amd64")
	return strings.ReplaceAll(abi, ":x86:32", ":i386")
}

func parseCatalog(b []byte, member string, local bool) (repoContent, error) {
	repo := repoContent{byName: make(map[string]manifest), extras: map[string]json.RawMessage{"groups": json.RawMessage("[]"), "expired_packages": json.RawMessage("[]")}}
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
		origin := packageBuildOrigin(m.Origin, m.Annotations.Flavor, m.Annotations.Subpackage)
		p := CatalogPackage{Path: m.Path, Name: m.Name, Version: m.Version, Origin: m.Origin, BuildOrigin: origin, Sum: m.Sum, Size: *m.Pkgsize, Local: local, ABI: packageABI(m.ABI, m.Arch)}
		p.overlayDigest = m.Annotations.OverlayDigest
		if old, ok := repo.byName[p.Name]; ok && comparePkgVersions(old.pkg.Version, p.Version) >= 0 {
			return nil
		}
		repo.byName[p.Name] = manifest{pkg: p, raw: raw, provides: m.Provides}
		return nil
	}
	if member == "data" {
		var data map[string]json.RawMessage
		if err := json.Unmarshal(b, &data); err != nil {
			return repo, err
		}
		d := json.NewDecoder(bytes.NewReader(data["packages"]))
		if tok, err := d.Token(); err != nil || tok != json.Delim('[') {
			return repo, errors.New("data catalog missing packages array")
		}
		for d.More() {
			var raw json.RawMessage
			if err := d.Decode(&raw); err != nil {
				return repo, err
			}
			if err := add(raw); err != nil {
				return repo, err
			}
		}
		delete(data, "packages")
		for k, v := range data {
			repo.extras[k] = v
		}
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

func compatibleABI(a, b string) bool {
	if a == b {
		return true
	}
	if prefix, ok := strings.CutSuffix(a, ":*"); ok {
		return strings.HasPrefix(b, prefix+":")
	}
	if prefix, ok := strings.CutSuffix(b, ":*"); ok {
		return strings.HasPrefix(a, prefix+":")
	}
	return false
}

const repoMeta = "version = 2;\npacking_format = \"tzst\";\nmanifests = \"packagesite.yaml\";\nmanifests_archive = \"packagesite\";\ndata = \"data\";\n"

func (c *Catalog) Refresh(ctx context.Context) error {
	c.refresh.Lock()
	defer c.refresh.Unlock()
	check, info, err := c.openLocalIndex()
	if err != nil {
		return fmt.Errorf("open local Poudriere catalog: %w", err)
	}
	if check != nil {
		check.Close()
	}
	var previous catalogRevision
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
	index, info, err := c.openLocalIndex()
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
	return c.publishCatalog(ctx, repo, revision, index, info, c.live)
}

// publishCatalog builds both signed indexes before making any input or output
// visible. The caller holds publish. A failed rebuild leaves the last good
// upstream, local packages and signed snapshot available for retry.
func (c *Catalog) publishCatalog(ctx context.Context, upstream *repoContent, revision catalogRevision, index *os.File, info os.FileInfo, live map[string]livePackage) error {
	repo := repoContent{byName: maps.Clone(upstream.byName), extras: upstream.extras}
	var local repoContent
	if index != nil {
		member := "packagesite.yaml"
		if strings.HasPrefix(filepath.Base(index.Name()), "data.") {
			member = "data"
		}
		m, err := unpackCatalog(index, member)
		if err != nil {
			return fmt.Errorf("local Poudriere catalog: %w", err)
		}
		local, err = parseCatalog(m[member], member, true)
		if err != nil {
			return fmt.Errorf("local Poudriere catalog: %w", err)
		}
	}
	localFiles := make(map[string]os.FileInfo, len(local.byName))
	if len(local.byName) != 0 {
		localFiles[".pkg-cache"], _ = os.Stat(filepath.Join(c.localDir, ".pkg-cache"))
	}
	s := &catalogSnapshot{
		overlays:      c.overlays,
		byPath:        make(map[string]CatalogPackage, len(repo.byName)),
		pathByName:    make(map[string]string, len(repo.byName)),
		displaced:     make(map[string]CatalogPackage),
		compiled:      make(map[string]CatalogPackage, len(local.byName)),
		compiledAt:    make(map[string]time.Time, len(local.byName)),
		compiledFiles: make(map[string]*compiledArchive, len(local.byName)),
		fetched:       make(map[string]bool),
		metadata:      make(map[string]metadataFile),
	}
	arches := map[string]bool{}
	for name, m := range repo.byName {
		if i := strings.LastIndexByte(m.pkg.ABI, ':'); i > 0 {
			arches[m.pkg.ABI] = true
			arches[m.pkg.ABI[:i]+":*"] = true
		}
		if _, overlaid := c.overlays[basePort(m.pkg.Origin)]; overlaid {
			s.displaced[name] = m.pkg
			delete(repo.byName, name)
		}
	}
	for name, m := range local.byName {
		if !arches[m.pkg.ABI] {
			continue
		}
		if !s.permitsLocal(m.pkg, false) {
			continue
		}
		old, exists := repo.byName[name]
		if exists && !compatibleABI(m.pkg.ABI, old.pkg.ABI) {
			continue
		}
		versionOrder := comparePkgVersions(m.pkg.Version, old.pkg.Version)
		preferred := !exists || versionOrder > 0
		stat, err := os.Stat(filepath.Join(c.localDir, m.pkg.Path))
		localFiles[m.pkg.Path] = stat
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			if !preferred {
				continue
			}
			return err
		}
		if !stat.Mode().IsRegular() || stat.Size() != m.pkg.Size {
			if !preferred {
				continue
			}
			return fmt.Errorf("local package %s differs from its catalog", m.pkg.Path)
		}
		// Poudriere can satisfy build dependencies with prebuilt packages.
		// It hardlinks these from .pkg-cache into the local All directory.
		fetched, err := fetchedByPoudriere(c.localDir, m.pkg.Path, stat)
		if err != nil {
			if preferred {
				return fmt.Errorf("identify native package %s: %w", m.pkg.Path, err)
			}
			continue
		}
		if fetched && !s.permitsLocal(m.pkg, true) {
			continue
		}
		if !fetched {
			file := &compiledArchive{path: filepath.Join(c.localDir, m.pkg.Path), info: stat}
			if prev := c.current.Load(); prev != nil && prev.compiled[name] == m.pkg {
				if prior := prev.compiledFiles[name]; prior != nil && prior.path == file.path && sameArchive(prior.info, stat) {
					file = prior
				}
			}
			// Validate custom builds and allow a verified native build to win
			// a version tie with the FreeBSD package.
			if m.pkg.overlayDigest != "" || exists && versionOrder == 0 && m.pkg.BuildOrigin == old.pkg.BuildOrigin {
				if err := file.verify(m.pkg); err != nil {
					log.Printf("skipping invalid compiled package %s: %v", m.pkg.Path, err)
					continue
				}
				preferred = true
			}
			s.compiled[name] = m.pkg
			s.compiledAt[name] = stat.ModTime()
			s.compiledFiles[name] = file
		}
		if !preferred {
			continue
		}
		if fetched {
			s.fetched[name] = true
		}
		repo.byName[name] = m
		if exists {
			s.displaced[name] = old.pkg
		}
	}
	var baseLibraries map[string]bool
	if len(live) > 0 {
		baseLibraries = jailBaseLibraries(c.jailRoot)
		mergeLivePackages(live, repo.byName, arches, baseLibraries, s)
	}
	// Emit the same complete manifests in both pkg formats, including fields
	// introduced after this software was built.
	names := make([]string, 0, len(repo.byName))
	for k := range repo.byName {
		names = append(names, k)
	}
	sort.Strings(names)
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
		m := repo.byName[k]
		if _, exists := s.byPath[m.pkg.Path]; exists {
			return fmt.Errorf("duplicate catalog archive path %q", m.pkg.Path)
		}
		s.byPath[m.pkg.Path] = m.pkg
		s.pathByName[m.pkg.Name] = m.pkg.Path
		if original, ok := s.displaced[k]; ok && !s.fetched[k] && comparePkgVersions(m.pkg.Version, original.Version) > 0 {
			s.ahead++
		}
		if m.pkg.Name == "pkg" {
			s.bootstrapPath = m.pkg.Path
		}
		// Input packagesite rows are single-line; data catalogs may be indented.
		var compact bytes.Buffer
		if bytes.ContainsAny(m.raw, "\r\n") {
			if err := json.Compact(&compact, m.raw); err != nil {
				return err
			}
			m.raw = compact.Bytes()
		}
		lines.Write(m.raw)
		lines.WriteByte('\n')
		if i != 0 {
			data.WriteByte(',')
		}
		data.Write(m.raw)
	}
	data.WriteString("]}")
	addFile := func(name string, b []byte) {
		s.metadata[name] = metadataFile{b, fmt.Sprintf("\"%x\"", sha256.Sum256(b))}
	}
	addFile("meta.conf", []byte(repoMeta))
	for _, f := range []struct {
		archive, member string
		body            []byte
	}{{"meta.pkg", "meta", []byte(repoMeta)}, {"packagesite.pkg", "packagesite.yaml", lines.Bytes()}, {"data.pkg", "data", data.Bytes()}} {
		archive, err := signedPkg(c.key, f.member, f.body)
		if err != nil {
			return err
		}
		addFile(f.archive, archive)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.updated = time.Now().UTC()
	if prev := c.current.Load(); prev != nil {
		if s.metadata["data.pkg"].etag == prev.metadata["data.pkg"].etag {
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
	if err := checkUpstreamFingerprint(c.fingerprints, revision.fingerprint); err != nil {
		return err
	}
	if err := c.retainLiveDownloads(s, live); err != nil {
		return err
	}
	c.revision, c.upstreamContent, c.localIndex, c.localArchives = revision, upstream, info, localFiles
	c.live = maps.Clone(live)
	c.baseLibraries = baseLibraries
	c.current.Store(s)
	return nil
}
