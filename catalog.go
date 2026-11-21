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
}

type manifest struct {
	pkg CatalogPackage
	raw json.RawMessage
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
	bootstrapPath string
	byPath        map[string]CatalogPackage
	pathByName    map[string]string
	// Only the FreeBSD packages displaced by local builds need a second copy.
	displaced     map[string]CatalogPackage
	compiled      map[string]CatalogPackage
	compiledAt    map[string]time.Time
	compiledFiles map[string]*compiledArchive
	fetched       map[string]bool
	metadata      map[string]metadataFile
	updated       time.Time
	ahead         int
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

type Catalog struct {
	upstream               *url.URL
	fingerprints, localDir string
	client                 *http.Client
	key                    *rsa.PrivateKey
	PublicKey              []byte
	refresh                sync.Mutex
	revision               catalogRevision // last successfully published inputs, guarded by refresh
	localIndex             os.FileInfo
	localArchives          map[string]os.FileInfo
	current                atomic.Pointer[catalogSnapshot]
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
	return &Catalog{upstream: u, fingerprints: fingerprintsDir, localDir: localRepoDir, client: client, key: key, PublicKey: publicKey}, nil
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
			Annotations                                           struct{ Flavor, Subpackage string }
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
		if old, ok := repo.byName[p.Name]; ok && comparePkgVersions(old.pkg.Version, p.Version) >= 0 {
			return nil
		}
		repo.byName[p.Name] = manifest{pkg: p, raw: raw}
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
	index, info, err := c.openLocalIndex()
	if err != nil {
		return fmt.Errorf("open local Poudriere catalog: %w", err)
	}
	if index != nil {
		defer index.Close()
	}
	var previous catalogRevision
	if c.revision.archive != "" && c.localInputsUnchanged(info) {
		previous = c.revision
	}
	repo, revision, err := c.fetchCatalog(ctx, previous)
	if err != nil {
		return err
	}
	if repo == nil {
		// Revalidation or an identical verified signature confirmed the snapshot.
		c.revision = revision
		return nil
	}
	var local repoContent
	if index != nil {
		m, err := unpackCatalog(index, "packagesite.yaml")
		if err != nil {
			return fmt.Errorf("local Poudriere catalog: %w", err)
		}
		local, err = parseCatalog(m["packagesite.yaml"], "packagesite.yaml", true)
		if err != nil {
			return fmt.Errorf("local Poudriere catalog: %w", err)
		}
	}
	localFiles := make(map[string]os.FileInfo, len(local.byName))
	if len(local.byName) != 0 {
		localFiles[".pkg-cache"], _ = os.Stat(filepath.Join(c.localDir, ".pkg-cache"))
	}
	s := &catalogSnapshot{
		byPath:        make(map[string]CatalogPackage, len(repo.byName)),
		pathByName:    make(map[string]string, len(repo.byName)),
		displaced:     make(map[string]CatalogPackage),
		compiled:      make(map[string]CatalogPackage, len(local.byName)),
		compiledAt:    make(map[string]time.Time, len(local.byName)),
		compiledFiles: make(map[string]*compiledArchive, len(local.byName)),
		fetched:       make(map[string]bool),
		metadata:      make(map[string]metadataFile),
		updated:       time.Now().UTC(),
	}
	arches := map[string]bool{}
	for _, m := range repo.byName {
		if i := strings.LastIndexByte(m.pkg.ABI, ':'); i > 0 {
			arches[m.pkg.ABI] = true
			arches[m.pkg.ABI[:i]+":*"] = true
		}
	}
	for name, m := range local.byName {
		if !arches[m.pkg.ABI] {
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
		if fetched {
			s.fetched[name] = true
		} else if err == nil {
			file := &compiledArchive{path: filepath.Join(c.localDir, m.pkg.Path), info: stat}
			if prev := c.current.Load(); prev != nil && prev.compiled[name].Sum == m.pkg.Sum {
				if prior := prev.compiledFiles[name]; prior != nil && prior.path == file.path && sameArchive(prior.info, stat) {
					file = prior
				}
			}
			// Publish the compiled manifest (including its checksum) on a tie.
			if exists && versionOrder == 0 && m.pkg.BuildOrigin == old.pkg.BuildOrigin {
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
		repo.byName[name] = m
		if exists {
			s.displaced[name] = old.pkg
			if versionOrder > 0 {
				s.ahead++
			}
		}
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
	if prev := c.current.Load(); prev != nil && s.metadata["data.pkg"].etag == prev.metadata["data.pkg"].etag {
		s.updated = prev.updated
	}
	c.revision, c.localIndex, c.localArchives = revision, info, localFiles
	c.current.Store(s)
	return nil
}
