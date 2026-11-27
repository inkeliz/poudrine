package public

import (
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/inkeliz/poudrine/internal/statusproto"
)

func TestPublicStatsPinnedReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	writer, err := OpenStatsWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	buf := make([]byte, 1024)
	for _, counts := range []statusproto.Counts{
		{Packages: math.MaxInt64, Ahead: math.MaxInt64, LocalBuilds: new(int64(math.MaxInt64)), Compilations: &statusproto.Compilations{
			UTCDate: 20261127, Today: math.MaxInt64 / 2, Yesterday: math.MaxInt64 / 2, Last7Days: math.MaxInt64,
		}},
		{Packages: 1, LocalBuilds: new(int64), Compilations: &statusproto.Compilations{UTCDate: 20261127}},
		{},
	} {
		if err := WritePublicStats(writer, counts); err != nil {
			t.Fatal(err)
		}
		if other, err := OpenStatsWriter(path); err == nil {
			other.Close()
			t.Fatal("second writer acquired the snapshot")
		}
		n, err := reader.ReadAt(buf, 0)
		if err != io.EOF {
			t.Fatal(err)
		}
		if n != statusproto.RecordSize {
			t.Fatalf("fixed-size snapshot: got %d bytes", n)
		}
		if got, ok := statusproto.Parse(buf[:n]); !ok || !reflect.DeepEqual(got, counts) {
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
		if w, err := OpenStatsWriter(p); err == nil {
			w.Close()
			t.Fatal("accepted", name, "link")
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if w, err := OpenStatsWriter(private); err == nil {
		w.Close()
		t.Fatal("accepted existing private file")
	}
	if got, err := os.ReadFile(private); err != nil || string(got) != "private" {
		t.Fatal("private file was modified", err)
	}
}
