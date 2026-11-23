package catalog

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestStreamingReadersReplaySeekAndSurviveOwnerCancellation(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 8192)
	finish := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		select {
		case <-finish:
			w.Write(data[len(data)/2:])
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(up.Close)
	t.Cleanup(release)
	u, _ := url.Parse(up.URL)
	cache, err := NewCache(t.TempDir(), 0, u, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	p := testutil.Package("pkg", string(data))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ownerCtx, cancelOwner := context.WithCancel(ctx)
	defer cancelOwner()
	first, err := cache.Stream(ownerCtx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	prefix := make([]byte, 4096)
	if _, err := io.ReadFull(first, prefix); err != nil || !bytes.Equal(prefix, data[:len(prefix)]) {
		t.Fatalf("first reader waited for completion: %v", err)
	}
	second, err := cache.Stream(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := io.ReadFull(second, prefix); err != nil || !bytes.Equal(prefix, data[:len(prefix)]) {
		t.Fatalf("late reader did not start at zero: %v", err)
	}
	cancelOwner()
	first.Close()
	if _, err := second.Seek(int64(len(data)-17), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(second); tail <- b }()
	select {
	case <-tail:
		t.Fatal("future range completed before upstream bytes arrived")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	if got := <-tail; !bytes.Equal(got, data[len(data)-17:]) {
		t.Fatalf("range/replay failed: %q", got)
	}
	third, err := cache.Stream(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(third)
	third.Close()
	if err != nil || !bytes.Equal(got, data) || hits.Load() != 1 {
		t.Fatalf("completed shared replay: %d hits, %v", hits.Load(), err)
	}
	second.Close()
	if _, err := os.Stat(second.download.file.Name()); !os.IsNotExist(err) {
		t.Fatalf("temporary file survived the last reader: %v", err)
	}
}

func TestStreamingLastReaderCancelsUpstream(t *testing.T) {
	stopped := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	cache, err := NewCache(t.TempDir(), 100, u, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	p := testutil.Package("pkg", string(bytes.Repeat([]byte{'a'}, 100)))
	reader, err := cache.Stream(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("unused upstream remained open")
	}
}
