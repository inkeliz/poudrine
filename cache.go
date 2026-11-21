package poudrine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// Only completed, verified downloads are named by a digest. Open descriptors
// stay readable by clients even after eviction on FreeBSD.
type cachedArchive struct {
	Key     string
	Package CatalogPackage
	Used    time.Time
}

type packageCache struct {
	dir      string
	limit    int64
	upstream *url.URL
	client   *http.Client
	fill     chan struct{}
	mu       sync.Mutex
	files    map[string]cachedArchive
	catalog  atomic.Pointer[catalogSnapshot]
	bytes    int64
}

func newPackageCache(dir string, limit int64, upstream *url.URL, client *http.Client) (*packageCache, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	c := &packageCache{dir: dir, limit: limit, upstream: upstream, client: client, fill: make(chan struct{}, 1), files: make(map[string]cachedArchive)}
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
		c.files[name] = cachedArchive{Key: name, Package: CatalogPackage{Size: info.Size()}, Used: info.ModTime()}
		c.bytes += info.Size()
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
func (c *packageCache) makeRoom(size int64) error {
	if c.bytes <= c.limit-size {
		return nil
	}
	entries := make([]cachedArchive, 0, len(c.files))
	for _, e := range c.files {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Used.Before(entries[j].Used) })
	for _, e := range entries {
		if err := c.remove(e); err != nil {
			return err
		}
		if c.bytes <= c.limit-size {
			break
		}
	}
	return nil
}

// remove is called with mu held, or during startup.
func (c *packageCache) remove(e cachedArchive) error {
	if err := os.Remove(filepath.Join(c.dir, e.Key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(c.files, e.Key)
	c.bytes -= e.Package.Size
	return nil
}

func (s *catalogSnapshot) supersedes(p CatalogPackage) bool {
	if s == nil {
		return false
	}
	local, ok := s.compiled[p.Name]
	if !ok || p.Version == "" || local.Origin != p.Origin || local.BuildOrigin != p.BuildOrigin ||
		!compatibleABI(local.ABI, p.ABI) || comparePkgVersions(local.Version, p.Version) < 0 {
		return false
	}
	file := s.compiledFiles[p.Name]
	return file != nil && file.verify(local) == nil
}

func (c *packageCache) pruneCompiled(s *catalogSnapshot) error {
	c.catalog.Store(s)
	if len(s.compiled) == 0 {
		return nil
	}
	var errs error
	for _, e := range c.inventory(s) {
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

func (c *packageCache) hit(key string, p CatalogPackage) (*os.File, error) {
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
		c.bytes -= e.Package.Size
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

func verifyPackage(src io.Reader, dest io.Writer, p CatalogPackage) error {
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

func packageCacheKey(sum string) string {
	digest := sha256.Sum256([]byte(sum))
	return hex.EncodeToString(digest[:])
}

func (c *packageCache) open(ctx context.Context, p CatalogPackage) (*os.File, error) {
	key := packageCacheKey(p.Sum)
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
	if err := verifyPackage(resp.Body, f, p); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if p.Size <= c.limit && c.limit > 0 {
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
		if err := f.Sync(); err != nil {
			return nil, err
		}
		if err := os.Rename(f.Name(), filepath.Join(c.dir, key)); err != nil {
			return nil, err
		}
		c.files[key] = cachedArchive{Key: key, Package: p, Used: time.Now()}
		c.bytes += p.Size
	}
	ready = true
	return f, nil
}
