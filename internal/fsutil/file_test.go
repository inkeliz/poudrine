package fsutil

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAtomicPublication(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity")
	first, second := []byte("first complete value"), []byte("second complete value")
	start := make(chan struct{})
	var group sync.WaitGroup
	for _, data := range [][]byte{first, second} {
		group.Go(func() {
			<-start
			if err := CreateFileOnce(path, data, 0600); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	group.Wait()
	winner, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(winner, first) && !bytes.Equal(winner, second) {
		t.Fatalf("first publisher: %q, %v", winner, err)
	}
	if err := CreateFileOnce(path, []byte("retry"), 0644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, winner) {
		t.Fatalf("retry replaced identity: %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("identity mode: %v, %v", info, err)
	}
	if err := WriteAtomicFile(path, []byte("updated"), 0644); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "updated" {
		t.Fatalf("replace: %q, %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files left in publish directory: %v, %v", entries, err)
	}
}
