package poudrine

import (
	"context"
	"errors"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const liveArchiveDirName = "live-packages"
const liveURLPrefix = "All/poudrine/"
const liveDownloadGrace = 24 * time.Hour

// Only the package URLs selected for a signed catalog get a download link.
// A client using that catalog can finish fetching after Poudriere rotates
// its repository, even if the web service has since restarted.
func (c *Catalog) retainLiveDownloads(s *catalogSnapshot, live map[string]livePackage) error {
	dir := filepath.Join(c.liveDir, "published")
	previous := c.current.Load()
	created := false
	for name, candidate := range live {
		p := candidate.manifest.pkg
		if !s.byPath[p.Path].live {
			continue
		}
		if previous != nil && previous.byPath[p.Path].live && c.live[name].file == candidate.file {
			continue
		}
		if !created {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return err
			}
			created = true
		}
		file := candidate.file
		dst := filepath.Join(dir, filepath.Base(p.Path))
		if err := os.Link(file.path, dst); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err := os.Lstat(dst)
			if err != nil {
				return err
			}
			if !os.SameFile(file.info, info) {
				// Capture can replace an invalid copy with a repaired archive.
				if err := os.Remove(dst); err != nil {
					return err
				}
				if err := os.Link(file.path, dst); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func liveDownloadDigest(urlPath string) string {
	name, ok := strings.CutPrefix(urlPath, liveURLPrefix)
	sum, ext := strings.CutSuffix(name, ".pkg")
	if !ok || !ext || len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
		return ""
	}
	return sum
}

func (c *Catalog) openLiveDownload(sum string) (*os.File, error) {
	if c.liveDir == "" || sum == "" {
		return nil, os.ErrNotExist
	}
	flags := os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	f, err := os.OpenFile(filepath.Join(c.liveDir, "published", sum+".pkg"), flags, 0)
	if errors.Is(err, os.ErrNotExist) {
		return os.OpenFile(filepath.Join(c.liveDir, "retired", sum+".pkg"), flags, 0)
	}
	return f, err
}

func sameBuilderRepository(o options) bool {
	if o.localRepo == o.builder.RepositoryDir() {
		return true
	}
	local, err := os.Stat(o.localRepo)
	if err != nil {
		return false
	}
	native, err := os.Stat(o.builder.RepositoryDir())
	return err == nil && os.SameFile(local, native)
}

func monitorLivePackages(ctx context.Context, o options, cat *Catalog) {
	store := liveArchiveStore{dir: cat.liveDir, repoDir: o.localRepo}
	var candidates, submitted map[string]livePackage
	var lastSweep time.Time
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		var err error
		candidates = nil
		snapshot := cat.current.Load()
		if snapshot != nil && sameBuilderRepository(o) {
			store.offeredVersion = snapshot.offeredVersion
			candidates, err = store.scan(ctx, o.builder)
		}
		if err != nil {
			candidates = submitted
		}
		changed := !maps.EqualFunc(submitted, candidates, func(a, b livePackage) bool { return a.file == b.file && a.fetched == b.fetched })
		if snapshot != nil && (changed || cat.publicationInputsChanged()) {
			if publishErr := cat.PublishLive(ctx, candidates); publishErr == nil {
				submitted = candidates
			} else {
				err = errors.Join(err, publishErr)
			}
		}
		cat.recordLiveScan(store.failures, err)
		if err != nil && ctx.Err() == nil {
			log.Printf("publish completed packages: %v", err)
		}
		now := time.Now()
		if now.Sub(lastSweep) >= time.Hour {
			if err := pruneLiveDownloads(cat, candidates, now); err != nil {
				log.Printf("clean completed package archives: %v", err)
			}
			lastSweep = now
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Age from the first sweep that sees an unused archive. Packages may have
// been available for weeks during a long build before their final handoff.
func pruneLiveDownloads(cat *Catalog, candidates map[string]livePackage, now time.Time) error {
	keep := make(map[string]bool, len(candidates))
	for _, entry := range candidates {
		keep[filepath.Base(entry.file.path)] = true
	}
	// A failed catalog refresh may still be using the previous candidate set.
	// Hold the publication lock while unlinking so a new snapshot cannot
	// make a package visible midway through the sweep.
	cat.publish.Lock()
	defer cat.publish.Unlock()
	for _, entry := range cat.live {
		keep[filepath.Base(entry.file.path)] = true
	}
	published := filepath.Join(cat.liveDir, "published")
	retired := filepath.Join(cat.liveDir, "retired")
	for _, dir := range []string{cat.liveDir, published, retired} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.Type().IsRegular() || (liveDownloadDigest(liveURLPrefix+name) == "" && !strings.HasPrefix(name, ".capture-")) {
				continue
			}
			if dir != retired && keep[name] {
				continue
			}
			path := filepath.Join(dir, name)
			switch dir {
			case published:
				// The retirement time survives restarts. Old clients can use
				// the same URL throughout the atomic move into the grace period.
				if err := os.MkdirAll(retired, 0700); err != nil {
					return err
				}
				if err := os.Chtimes(path, now, now); err != nil {
					return err
				}
				if err := os.Rename(path, filepath.Join(retired, name)); err != nil {
					return err
				}
				continue
			case retired:
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if now.Sub(info.ModTime()) < liveDownloadGrace {
					continue
				}
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
