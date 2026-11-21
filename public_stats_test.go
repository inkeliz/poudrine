package poudrine

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPublicStatsPinnedReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	writer, err := openStatsWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	updated := time.Now()
	buf := make([]byte, 1024)
	for _, counts := range []CatalogStats{{1234567, 99, &updated}, {1, 0, &updated}, {0, 0, &updated}} {
		if err := writePublicStats(writer, counts); err != nil {
			t.Fatal(err)
		}
		if other, err := openStatsWriter(path); err == nil {
			other.Close()
			t.Fatal("second writer acquired the snapshot")
		}
		n, err := reader.ReadAt(buf, 0)
		if err != io.EOF {
			t.Fatal(err)
		}
		var got map[string]int
		if err := json.Unmarshal(buf[:n], &got); err != nil {
			t.Fatal(err)
		}
		packages, totalOK := got["packages"]
		ahead, aheadOK := got["ahead"]
		if len(got) != 2 || !totalOK || !aheadOK || packages != counts.Packages || ahead != counts.Ahead {
			t.Fatalf("public snapshot %s", buf[:n])
		}
	}
}

func TestPublicStatsWriterRejectsLinksAndPrivateFile(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "key")
	if err := os.WriteFile(private, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, link := range map[string]func(string, string) error{"symbolic": os.Symlink, "hard": os.Link} {
		p := filepath.Join(dir, name)
		if err := link(private, p); err != nil {
			t.Fatal(err)
		}
		if w, err := openStatsWriter(p); err == nil {
			w.Close()
			t.Fatal("accepted", name, "link")
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if w, err := openStatsWriter(private); err == nil {
		w.Close()
		t.Fatal("accepted existing private file")
	}
	if got, err := os.ReadFile(private); err != nil || string(got) != "private" {
		t.Fatal("private file was modified", err)
	}
}
