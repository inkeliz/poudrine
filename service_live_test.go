package poudrine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveDownloadRetention(t *testing.T) {
	c := &Catalog{liveDir: t.TempDir()}
	makePackage := func(name, sum string) livePackage {
		p := CatalogPackage{Name: name, Path: "All/poudrine/" + sum + ".pkg", Local: true, live: true}
		path := filepath.Join(c.liveDir, sum+".pkg")
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return livePackage{manifest: manifest{pkg: p}, file: &compiledArchive{path: path, info: info}}
	}
	active := makePackage("ready", strings.Repeat("a", 64))
	blocked := makePackage("waiting", strings.Repeat("b", 64))
	ready := active.manifest.pkg
	c.live = map[string]livePackage{ready.Name: active, "waiting": blocked}
	s := &catalogSnapshot{
		byPath: map[string]CatalogPackage{ready.Path: ready},
	}
	if err := c.retainLiveDownloads(s, c.live); err != nil {
		t.Fatal(err)
	}
	hasDownload := func(p CatalogPackage, want bool) {
		t.Helper()
		f, err := c.openLiveDownload(liveDownloadDigest(p.Path))
		if err == nil {
			f.Close()
		}
		if (err == nil) != want || err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("download %s: want available=%t, got %v", p.Name, want, err)
		}
	}
	start := time.Now()
	sweep := func(at time.Time) {
		t.Helper()
		if err := pruneLiveDownloads(c, nil, at); err != nil {
			t.Fatal(err)
		}
	}
	sweep(start)
	hasDownload(blocked.manifest.pkg, false)
	end := start.Add(7 * 24 * time.Hour)
	sweep(end)
	hasDownload(ready, true)
	c.live = nil
	sweep(end)
	if _, err := os.Stat(active.file.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired staging file: %v", err)
	}
	sweep(end.Add(liveDownloadGrace - time.Second))
	hasDownload(ready, true)
	sweep(end.Add(liveDownloadGrace))
	hasDownload(ready, false)
}
