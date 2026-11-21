package poudrine

import (
	"archive/tar"
	"encoding/json"
	"io"
	"os"
	"strings"
	"syscall"
)

// pkg writes the compact manifest at the beginning of the archive. Read
// only a bounded header region to recover older cache entries on startup.
func cachedPackageIdentity(path string, size int64) CatalogPackage {
	p := CatalogPackage{Size: size}
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
		p.ABI = packageABI(m.ABI, m.Arch)
		p.BuildOrigin = packageBuildOrigin(m.Origin, m.Annotations.Flavor, m.Annotations.Subpackage)
		return p
	}
}

func (c *packageCache) inventory(s *catalogSnapshot) []cachedArchive {
	c.mu.Lock()
	entries := make([]cachedArchive, 0, len(c.files))
	var unknown map[string]int
	for _, e := range c.files {
		// The current signed checksum can supply an ABI missing from an old manifest.
		if e.Package.Name != "" && e.Package.ABI == "" {
			if p, ok := s.freeBSDPackage(e.Package.Name); ok && p.Size == e.Package.Size && packageCacheKey(p.Sum) == e.Key {
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
		for name := range s.pathByName {
			p, ok := s.freeBSDPackage(name)
			if !ok {
				continue
			}
			key := packageCacheKey(p.Sum)
			if i, exists := unknown[key]; exists && entries[i].Package.Size == p.Size {
				entries[i].Package = p
				c.mu.Lock()
				if entry, stillCached := c.files[key]; stillCached && entry.Package.Name == "" {
					entry.Package = p
					c.files[key] = entry
				}
				c.mu.Unlock()
				delete(unknown, key)
				if len(unknown) == 0 {
					break
				}
			}
		}
	}
	return entries
}
