package poudrine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPoudriereDiskUsage(t *testing.T) {
	dir := t.TempDir()
	cfg := workerTestConfig(dir)
	for _, path := range []string{cfg.BaseFS, cfg.Distfiles} {
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	pkg := filepath.Join(cfg.BaseFS, "archive")
	if err := os.WriteFile(pkg, make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(pkg, filepath.Join(cfg.Distfiles, "hardlink")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "unrelated")
	if err := os.WriteFile(outside, make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.BaseFS, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	sparse := filepath.Join(cfg.Distfiles, "sparse")
	if err := os.WriteFile(sparse, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(sparse, 1<<28); err != nil {
		t.Fatal(err)
	}
	var want int64
	for _, path := range []string{cfg.BaseFS, cfg.Distfiles, pkg, link, sparse} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		want += info.Sys().(*syscall.Stat_t).Blocks * 512
	}
	alias := filepath.Join(dir, "base-link")
	if err := os.Symlink(cfg.BaseFS, alias); err != nil {
		t.Fatal(err)
	}
	cfg.BaseFS = alias
	if total, err := poudriereDiskUsage(t.Context(), cfg); err != nil || total != want {
		t.Fatalf("usage = %d, want %d: %v", total, want, err)
	}
	cfg.BaseFS = filepath.Join(dir, "missing")
	if _, err := poudriereDiskUsage(t.Context(), cfg); err == nil {
		t.Fatal("unreadable storage returned a partial total")
	}
}

func TestStoragePublishedByWorker(t *testing.T) {
	cfg := workerTestConfig(t.TempDir())
	for _, path := range []string{cfg.BaseFS, cfg.Distfiles} {
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := poudriereDiskUsage(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := &Builder{cfg: cfg}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		b.monitorStorage(ctx)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for b.Status().PoudriereBytes == nil {
		if time.Now().After(deadline) {
			t.Fatal("worker did not publish disk usage")
		}
		time.Sleep(5 * time.Millisecond)
	}
	b.begin(time.Now(), "scheduled", nil, time.Time{}, true)
	s, err := ReadBuildStatus(cfg.StatusFile)
	if err != nil || s.PoudriereBytes == nil || *s.PoudriereBytes != expected {
		t.Fatalf("starting the next build lost the measured disk usage: %+v, %v", s, err)
	}
}

func TestStorageScanMounts(t *testing.T) {
	var mounts []unix.Statfs_t
	for _, item := range []struct{ path, kind string }{
		{"/base", "ufs"}, {"/base/build/job/usr/ports", "nullfs"}, {"/base/build/job/dev", "devfs"},
		{"/base/build/job/usr/ports/data", "ufs"}, {"/base/data/packages", "zfs"},
		{"/base/data/packages/archives", "zfs"}, {"/base/dist", "ufs"}, {"/base-other", "zfs"},
	} {
		var m unix.Statfs_t
		copy(m.Mntonname[:], item.path)
		copy(m.Fstypename[:], item.kind)
		mounts = append(mounts, m)
	}
	roots, boundaries := storageScanRoots([]string{"/base/dist", "/base", "/base"}, mounts)
	slices.Sort(roots)
	want := []string{"/base", "/base/data/packages", "/base/data/packages/archives", "/base/dist"}
	if !slices.Equal(roots, want) {
		t.Fatalf("scan roots = %v, want %v", roots, want)
	}
	wantBoundaries := []string{"/base/build/job/dev", "/base/build/job/usr/ports", "/base/data/packages", "/base/data/packages/archives", "/base/dist"}
	if !slices.Equal(boundaries, wantBoundaries) {
		t.Fatalf("counted mount boundaries = %v, want %v", boundaries, wantBoundaries)
	}
}
