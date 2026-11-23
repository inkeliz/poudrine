package catalog

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/inkeliz/poudrine/internal/packageinfo"
)

// Only completed, verified downloads are named by a digest. Open descriptors
// stay readable by clients even after eviction on FreeBSD.
type cachedArchive struct {
	Key     string
	Package packageinfo.Package
	Used    time.Time
}

type Cache struct {
	dir      string
	Limit    int64 `json:"-"`
	upstream *url.URL
	client   *http.Client
	fill     chan struct{}
	mu       sync.Mutex
	files    map[string]cachedArchive
	catalog  atomic.Pointer[Snapshot]
	Bytes    int64 `json:"-"`
}

func NewCache(dir string, limit int64, upstream *url.URL, client *http.Client) (*Cache, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	c := &Cache{dir: dir, Limit: limit, upstream: upstream, client: upstreamClient(client), fill: make(chan struct{}, 1), files: make(map[string]cachedArchive)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, d := range entries {
		if !d.Type().IsRegular() {
			continue
		}
		name := d.Name()
		if strings.HasPrefix(name, ".download-") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return nil, err
			}
			continue
		}
		if len(name) != 64 {
			continue
		}
		if _, err := hex.DecodeString(name); err != nil {
			continue
		}
		info, err := d.Info()
		if err != nil {
			return nil, err
		}
		c.files[name] = cachedArchive{Key: name, Package: packageinfo.Package{Size: info.Size()}, Used: info.ModTime()}
		c.Bytes += info.Size()
	}
	if err := c.makeRoom(0); err != nil {
		return nil, err
	}
	// The manifest travels with the archive, including on old installations
	// and after the upstream catalog has moved on to a newer version.
	for key, entry := range c.files {
		entry.Package = cachedPackageIdentity(filepath.Join(dir, key), entry.Package.Size)
		c.files[key] = entry
	}
	return c, nil
}

// makeRoom is called while holding mu, or at startup.
func (c *Cache) makeRoom(size int64) error {
	if c.Bytes <= c.Limit-size {
		return nil
	}
	entries := slices.SortedFunc(maps.Values(c.files), func(a, b cachedArchive) int {
		return a.Used.Compare(b.Used)
	})
	for _, e := range entries {
		if err := c.remove(e); err != nil {
			return err
		}
		if c.Bytes <= c.Limit-size {
			break
		}
	}
	return nil
}

// remove is called with mu held, or during startup.
func (c *Cache) remove(e cachedArchive) error {
	if err := os.Remove(filepath.Join(c.dir, e.Key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(c.files, e.Key)
	c.Bytes -= e.Package.Size
	return nil
}

func (s *Snapshot) supersedes(p packageinfo.Package) bool {
	if s == nil {
		return false
	}
	if _, overlaid := s.Overlays[packageinfo.BasePort(p.Origin)]; overlaid {
		return true
	}
	local, ok := s.Compiled[p.Name]
	if !ok || p.Version == "" || local.Origin != p.Origin || local.BuildOrigin != p.BuildOrigin ||
		!packageinfo.CompatibleABI(local.ABI, p.ABI) || ComparePkgVersions(local.Version, p.Version) < 0 {
		return false
	}
	file := s.compiledFiles[p.Name]
	return file != nil && file.verify(local) == nil
}

func (c *Cache) PruneCompiled(s *Snapshot) error {
	c.catalog.Store(s)
	if len(s.Compiled) == 0 && len(s.Overlays) == 0 {
		return nil
	}
	var errs error
	for _, e := range c.Inventory(s) {
		if s.supersedes(e.Package) {
			c.mu.Lock()
			if entry, ok := c.files[e.Key]; ok {
				errs = errors.Join(errs, c.remove(entry))
			}
			c.mu.Unlock()
		}
	}
	return errs
}

func (c *Cache) hit(key string, p packageinfo.Package) (*os.File, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.files[key]
	if !ok {
		return nil, nil
	}
	path := filepath.Join(c.dir, key)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		delete(c.files, key)
		c.Bytes -= e.Package.Size
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if p.Size != e.Package.Size {
		f.Close()
		return nil, errUntrustedPackage
	}
	e.Used = time.Now()
	e.Package = p
	c.files[key] = e
	if err := os.Chtimes(path, e.Used, e.Used); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func VerifyPackage(src io.Reader, dest io.Writer, p packageinfo.Package) error {
	h, err := packageHasher(p.Sum)
	if err != nil {
		return err
	}
	if p.Size < 0 || p.Size >= 1<<62 {
		return errUntrustedPackage
	}
	n, err := io.Copy(io.MultiWriter(dest, h), io.LimitReader(src, p.Size+1))
	if err != nil {
		return err
	}
	if n != p.Size || !packageHashMatches(p.Sum, h) {
		return errUntrustedPackage
	}
	return nil
}

func PackageCacheKey(sum string) string {
	digest := sha256.Sum256([]byte(sum))
	return hex.EncodeToString(digest[:])
}

func (c *Cache) Open(ctx context.Context, p packageinfo.Package) (*os.File, error) {
	key := PackageCacheKey(p.Sum)
	if f, err := c.hit(key, p); f != nil || err != nil {
		return f, err
	}
	// Bound temporary storage and coalesce simultaneous misses for the same archive.
	select {
	case c.fill <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.fill }()
	if f, err := c.hit(key, p); f != nil || err != nil {
		return f, err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", c.upstream.JoinPath(p.Path).String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream package: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != p.Size {
		return nil, errUntrustedPackage
	}
	f, err := os.CreateTemp(c.dir, ".download-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	ready := false
	defer func() {
		if !ready {
			f.Close()
		}
	}()
	if err := VerifyPackage(resp.Body, f, p); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if p.Size <= c.Limit && c.Limit > 0 {
		if err := f.Sync(); err != nil {
			return nil, err
		}
		for {
			s := c.catalog.Load()
			// A download from the previous catalog can finish after publication.
			if s.supersedes(p) {
				ready = true
				return f, nil
			}
			c.mu.Lock()
			if s == c.catalog.Load() {
				break
			}
			c.mu.Unlock()
		}
		defer c.mu.Unlock()
		if err := c.makeRoom(p.Size); err != nil {
			return nil, err
		}
		if err := os.Rename(f.Name(), filepath.Join(c.dir, key)); err != nil {
			return nil, err
		}
		c.files[key] = cachedArchive{Key: key, Package: p, Used: time.Now()}
		c.Bytes += p.Size
	}
	ready = true
	return f, nil
}

// pkg writes the compact manifest at the beginning of the archive. Read
// only a bounded header region to recover older cache entries on startup.
func cachedPackageIdentity(path string, size int64) packageinfo.Package {
	p := packageinfo.Package{Size: size}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return p
	}
	defer f.Close()
	const limit = 4 << 20
	src, err := decompressArchive(io.LimitReader(f, limit))
	if err != nil {
		return p
	}
	defer src.Close()
	t := tar.NewReader(io.LimitReader(src, limit))
	for {
		h, err := t.Next()
		if err != nil {
			return p
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name != "+COMPACT_MANIFEST" && name != "+MANIFEST" {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size > limit {
			return p
		}
		var m struct {
			Name, Version, Origin, ABI, Arch string
			Annotations                      struct{ Flavor, Subpackage string }
		}
		if err := json.NewDecoder(t).Decode(&m); err != nil || m.Name == "" || m.Origin == "" || m.Version == "" {
			return p
		}
		p.Name, p.Origin, p.Version = m.Name, m.Origin, m.Version
		p.ABI = packageinfo.PackageABI(m.ABI, m.Arch)
		p.BuildOrigin = packageinfo.PackageBuildOrigin(m.Origin, m.Annotations.Flavor, m.Annotations.Subpackage)
		return p
	}
}

func (c *Cache) Inventory(s *Snapshot) []cachedArchive {
	c.mu.Lock()
	entries := make([]cachedArchive, 0, len(c.files))
	var unknown map[string]int
	for _, e := range c.files {
		// The current signed checksum can supply an ABI missing from an old manifest.
		if e.Package.Name != "" && e.Package.ABI == "" {
			if p, ok := s.FreeBSDPackage(e.Package.Name); ok && p.Size == e.Package.Size && PackageCacheKey(p.Sum) == e.Key {
				e.Package = p
				c.files[e.Key] = e
			}
		}
		if e.Package.Name == "" {
			if unknown == nil {
				unknown = make(map[string]int)
			}
			unknown[e.Key] = len(entries)
		}
		entries = append(entries, e)
	}
	c.mu.Unlock()
	if s != nil && len(unknown) != 0 {
		// An older or unreadable manifest can still be identified by the
		// signed upstream checksum, even if a newer local build is served.
		identify := func(p packageinfo.Package) {
			key := PackageCacheKey(p.Sum)
			if i, exists := unknown[key]; exists && entries[i].Package.Size == p.Size {
				entries[i].Package = p
				c.mu.Lock()
				if entry, stillCached := c.files[key]; stillCached && entry.Package.Name == "" {
					entry.Package = p
					c.files[key] = entry
				}
				c.mu.Unlock()
				delete(unknown, key)
			}
		}
		for _, p := range s.Displaced {
			identify(p)
			if len(unknown) == 0 {
				return entries
			}
		}
		for _, p := range s.ByPath {
			if !p.Local {
				identify(p)
			}
			if len(unknown) == 0 {
				break
			}
		}
	}
	return entries
}

var errUntrustedPackage = errors.New("package contents do not match the signed catalog")
