package catalog

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

	"github.com/inkeliz/poudrine/internal/config"
)

const LiveURLPrefix = "All/poudrine/"

const liveDownloadGrace = 24 * time.Hour

// Retain local-build provenance across restarts and the download grace period.
// Older links may have been published from Poudriere's binary fetch cache.
const LiveOverlayPrefix = "overlay-"

// Only the package URLs selected for a signed catalog get a download link.
// A client using that catalog can finish fetching after Poudriere rotates
// its repository, even if the web service has since restarted.
func (c *Catalog) retainLiveDownloads(s *Snapshot, live map[string]LivePackage) error {
	dir := filepath.Join(c.LiveDir, "published")
	previous := c.Current.Load()
	created := false
	for name, candidate := range live {
		p := candidate.Manifest.Pkg
		if !s.ByPath[p.Path].Live {
			continue
		}
		if previous != nil && previous.ByPath[p.Path].Live && c.Live[name].File == candidate.File {
			continue
		}
		if !created {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return err
			}
			created = true
		}
		file := candidate.File
		filename := filepath.Base(p.Path)
		if !candidate.Fetched && p.OverlayDigest != "" {
			filename = LiveOverlayPrefix + filename
		}
		dst := filepath.Join(dir, filename)
		if err := os.Link(file.Path, dst); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err := os.Lstat(dst)
			if err != nil {
				return err
			}
			if !os.SameFile(file.Info, info) {
				// Capture can replace an invalid copy with a repaired archive.
				if err := os.Remove(dst); err != nil {
					return err
				}
				if err := os.Link(file.Path, dst); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func LiveDownloadDigest(urlPath string) string {
	name, ok := strings.CutPrefix(urlPath, LiveURLPrefix)
	sum, ext := strings.CutSuffix(name, ".pkg")
	if !ok || !ext || len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
		return ""
	}
	return sum
}

func (c *Catalog) OpenLiveDownload(sum string) (*os.File, error) {
	if c.LiveDir == "" || sum == "" {
		return nil, os.ErrNotExist
	}
	flags := os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	prefixes := []string{""}
	if len(c.Overlays) != 0 {
		prefixes = []string{LiveOverlayPrefix, ""}
	}
	for _, prefix := range prefixes {
		for _, dir := range []string{"published", "retired"} {
			f, err := os.OpenFile(filepath.Join(c.LiveDir, dir, prefix+sum+".pkg"), flags, 0)
			if !errors.Is(err, os.ErrNotExist) {
				return f, err
			}
		}
	}
	return nil, os.ErrNotExist
}

func sameBuilderRepository(o config.Options) bool {
	if o.LocalRepo == o.Builder.RepositoryDir() {
		return true
	}
	local, err := os.Stat(o.LocalRepo)
	if err != nil {
		return false
	}
	native, err := os.Stat(o.Builder.RepositoryDir())
	return err == nil && os.SameFile(local, native)
}

func MonitorLivePackages(ctx context.Context, o config.Options, cat *Catalog) {
	store := LiveArchiveStore{Dir: cat.LiveDir, RepoDir: o.LocalRepo}
	var candidates, submitted map[string]LivePackage
	var lastSweep time.Time
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		var err error
		candidates = nil
		snapshot := cat.Current.Load()
		if snapshot != nil && sameBuilderRepository(o) {
			store.offeredVersion = snapshot.offeredVersion
			candidates, err = store.Scan(ctx, o.Builder)
		}
		if err != nil {
			candidates = submitted
		}
		changed := !maps.EqualFunc(submitted, candidates, func(a, b LivePackage) bool { return a.File == b.File && a.Fetched == b.Fetched })
		if snapshot != nil && (changed || cat.PublicationInputsChanged()) {
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
			if err := PruneLiveDownloads(cat, candidates, now); err != nil {
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
func PruneLiveDownloads(cat *Catalog, candidates map[string]LivePackage, now time.Time) error {
	keep := make(map[string]bool, len(candidates))
	for _, entry := range candidates {
		keep[filepath.Base(entry.File.Path)] = true
	}
	// A failed catalog refresh may still be using the previous candidate set.
	// Hold the publication lock while unlinking so a new snapshot cannot
	// make a package visible midway through the sweep.
	cat.publish.Lock()
	defer cat.publish.Unlock()
	for _, entry := range cat.Live {
		keep[filepath.Base(entry.File.Path)] = true
	}
	published := filepath.Join(cat.LiveDir, "published")
	retired := filepath.Join(cat.LiveDir, "retired")
	for _, dir := range []string{cat.LiveDir, published, retired} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			source := strings.TrimPrefix(name, LiveOverlayPrefix)
			if !entry.Type().IsRegular() || (LiveDownloadDigest(LiveURLPrefix+source) == "" && !strings.HasPrefix(name, ".capture-")) {
				continue
			}
			if dir != retired && keep[source] {
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
