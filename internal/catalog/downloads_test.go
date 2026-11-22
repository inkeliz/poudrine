package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/packageinfo"
)

func TestLiveDownloadRetention(t *testing.T) {
	c := &Catalog{LiveDir: t.TempDir()}
	makePackage := func(name, sum string) LivePackage {
		p := packageinfo.Package{Name: name, Path: "All/poudrine/" + sum + ".pkg", Local: true, Live: true}
		path := filepath.Join(c.LiveDir, sum+".pkg")
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return LivePackage{Manifest: manifest{Pkg: p}, File: &CompiledArchive{Path: path, Info: info}}
	}
	active := makePackage("ready", strings.Repeat("a", 64))
	blocked := makePackage("waiting", strings.Repeat("b", 64))
	ready := active.Manifest.Pkg
	c.Live = map[string]LivePackage{ready.Name: active, "waiting": blocked}
	s := &Snapshot{
		ByPath: map[string]packageinfo.Package{ready.Path: ready},
	}
	if err := c.retainLiveDownloads(s, c.Live); err != nil {
		t.Fatal(err)
	}
	hasDownload := func(p packageinfo.Package, want bool) {
		t.Helper()
		f, err := c.OpenLiveDownload(LiveDownloadDigest(p.Path))
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
		if err := PruneLiveDownloads(c, nil, at); err != nil {
			t.Fatal(err)
		}
	}
	sweep(start)
	hasDownload(blocked.Manifest.Pkg, false)
	end := start.Add(7 * 24 * time.Hour)
	sweep(end)
	hasDownload(ready, true)
	c.Live = nil
	sweep(end)
	if _, err := os.Stat(active.File.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired staging file: %v", err)
	}
	sweep(end.Add(liveDownloadGrace - time.Second))
	hasDownload(ready, true)
	sweep(end.Add(liveDownloadGrace))
	hasDownload(ready, false)
}
