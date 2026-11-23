package builder

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/inkeliz/poudrine/internal/config"
	"golang.org/x/sys/unix"
)

// Walking a ports tree can take a while. Only the worker measures it, off
// the build and HTTP request paths. Publish a complete result or nil.
func (b *Builder) monitorStorage(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		total, err := poudriereDiskUsage(ctx, b.Cfg)
		if ctx.Err() != nil {
			return
		}
		delay := 15 * time.Minute
		var size *int64
		if err != nil {
			log.Printf("measuring Poudriere disk usage: %v", err)
			delay = time.Minute // Files may disappear during a live build.
		} else {
			size = &total
		}
		b.mu.Lock()
		b.status.PoudriereBytes = size
		b.persistLocked()
		b.mu.Unlock()
		timer.Reset(delay)
	}
}

func poudriereDiskUsage(ctx context.Context, cfg config.Builder) (int64, error) {
	var roots []string
	for _, path := range []string{cfg.BaseFS, cfg.Distfiles} {
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return 0, err
		}
		roots = append(roots, realPath)
	}
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return 0, err
	}
	var mounts []unix.Statfs_t
	for {
		mounts = make([]unix.Statfs_t, n+8)
		n, err = unix.Getfsstat(mounts, unix.MNT_NOWAIT)
		if err != nil {
			return 0, err
		}
		if n < len(mounts) {
			break
		}
	}
	// -x prevents traversal of transient jail mounts during the scan. Pass
	// persistent nested filesystems as additional roots so separate package,
	// ports or jail datasets still count. One invocation deduplicates hardlinks.
	paths, boundaries := storageScanRoots(roots, mounts[:n])
	// FreeBSD du -x counts each mount's root inode in its parent's walk,
	// even though it skips the contents. Each included mount already gets
	// its own walk, and jail views should contribute no blocks at all.
	var boundaryBlocks int64
	for _, path := range boundaries {
		st, err := os.Lstat(path)
		if err != nil {
			return 0, err
		}
		stat, ok := st.Sys().(*syscall.Stat_t)
		if !ok || stat == nil {
			return 0, fmt.Errorf("missing filesystem metadata for %s", path)
		}
		boundaryBlocks += stat.Blocks
	}
	args := append([]string{"-scx"}, paths...)
	cmd := exec.CommandContext(ctx, "/usr/bin/du", args...)
	cmd.Env = []string{"BLOCKSIZE=512", "LC_ALL=C"}
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	result := strings.TrimSpace(string(out))
	last := result[strings.LastIndexByte(result, '\n')+1:]
	blocks, ok := strings.CutSuffix(last, "\ttotal")
	nblocks, err := strconv.ParseInt(blocks, 10, 64)
	if !ok || err != nil || nblocks < boundaryBlocks || nblocks > math.MaxInt64/512 {
		return 0, fmt.Errorf("unexpected du total %q", last)
	}
	return (nblocks - boundaryBlocks) * 512, nil
}

func storageScanRoots(roots []string, mounts []unix.Statfs_t) (paths, boundaries []string) {
	contains := func(dirs []string, path string) bool {
		return slices.ContainsFunc(dirs, func(dir string) bool {
			return dir == "/" || config.Within(path, dir)
		})
	}
	slices.Sort(roots)
	for _, root := range roots {
		if !contains(paths, root) {
			paths = append(paths, root)
		}
	}
	var ignored []string
	var nested []string
	for _, m := range mounts {
		path := unix.ByteSliceToString(m.Mntonname[:])
		if !contains(paths, path) || slices.Contains(paths, path) {
			continue
		}
		switch unix.ByteSliceToString(m.Fstypename[:]) {
		case "autofs", "devfs", "fdescfs", "linprocfs", "linsysfs", "nullfs", "procfs", "tmpfs", "unionfs":
			ignored = append(ignored, path)
		}
		nested = append(nested, path)
	}
	slices.Sort(nested)
	nested = slices.Compact(nested)
	for _, path := range nested {
		if contains(ignored, filepath.Dir(path)) {
			continue
		}
		boundaries = append(boundaries, path)
		if !slices.Contains(ignored, path) {
			paths = append(paths, path)
		}
	}
	return paths, boundaries
}
