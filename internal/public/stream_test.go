package public

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func streamingRepository(t *testing.T, corrupt bool) (*httptest.Server, *catalog.Cache, []byte, func(), *atomic.Int32) {
	t.Helper()
	data := bytes.Repeat([]byte("0123456789abcdef"), 8192)
	finish := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	hits := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		tail := append([]byte(nil), data[len(data)/2:]...)
		if corrupt {
			tail[len(tail)-1] ^= 1
		}
		w.Write(tail)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(release)
	u, _ := url.Parse(upstream.URL)
	cache, err := catalog.NewCache(t.TempDir(), 1<<20, u, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	p := testutil.Package("pkg", string(data))
	cat := &catalog.Catalog{}
	cat.Current.Store(&catalog.Snapshot{ByPath: map[string]packageinfo.Package{p.Path: p}})
	server := httptest.NewServer(&Repository{Catalog: cat, Cache: cache})
	server.Client().Timeout = 5 * time.Second
	t.Cleanup(server.Close)
	return server, cache, data, release, hits
}

func TestRepositoryStreamsSharedMissAndRanges(t *testing.T) {
	server, _, data, release, hits := streamingRepository(t, false)
	first, err := server.Client().Get(server.URL + "/All/pkg.pkg")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	prefix := make([]byte, 8192)
	if _, err := io.ReadFull(first.Body, prefix); err != nil || !bytes.Equal(prefix, data[:len(prefix)]) {
		t.Fatalf("did not proxy before completion: %v", err)
	}
	req, _ := http.NewRequest("GET", server.URL+"/All/pkg.pkg", nil)
	req.Header.Set("Range", "bytes=4-8195")
	ranged, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer ranged.Body.Close()
	if ranged.StatusCode != 206 || ranged.Header.Get("Content-Range") != fmt.Sprintf("bytes 4-8195/%d", len(data)) {
		t.Fatalf("range headers: %d %v", ranged.StatusCode, ranged.Header)
	}
	prefix = make([]byte, 8191)
	if _, err := io.ReadFull(ranged.Body, prefix); err != nil || !bytes.Equal(prefix, data[4:8195]) {
		t.Fatalf("range did not replay independently: %v", err)
	}
	tail := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(ranged.Body); tail <- b }()
	select {
	case <-tail:
		t.Fatal("range completed before archive verification")
	case <-time.After(20 * time.Millisecond):
	}
	first.Body.Close() // Remaining reader must keep the original upstream alive.
	release()
	if got := <-tail; !bytes.Equal(got, data[8195:8196]) {
		t.Fatalf("range final byte: %q", got)
	}
	for _, header := range []string{"bytes=-17", "bytes=0-2,100000-100009"} {
		req, _ := http.NewRequest("GET", server.URL+"/All/pkg.pkg", nil)
		req.Header.Set("Range", header)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 206 {
			t.Fatalf("%s: %d %v", header, response.StatusCode, err)
		}
		if header == "bytes=-17" {
			if !bytes.Equal(body, data[len(data)-17:]) {
				t.Fatal("wrong suffix")
			}
		} else {
			_, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			parts := multipart.NewReader(bytes.NewReader(body), params["boundary"])
			for _, want := range [][]byte{data[:3], data[100000:100010]} {
				part, err := parts.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(part)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("wrong multipart range: %q %v", got, err)
				}
			}
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("shared download fetched %d times", hits.Load())
	}
}

func TestRepositoryAbortsCorruptStreamWithoutCaching(t *testing.T) {
	for _, rangeHeader := range []string{"", "bytes=0-16383"} {
		t.Run(rangeHeader, func(t *testing.T) {
			server, cache, _, release, _ := streamingRepository(t, true)
			req, _ := http.NewRequest("GET", server.URL+"/All/pkg.pkg", nil)
			req.Header.Set("Range", rangeHeader)
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			want := 200
			if rangeHeader != "" {
				want = 206
			}
			if response.StatusCode != want {
				t.Fatalf("expected a live response, got %d", response.StatusCode)
			}
			prefix := make([]byte, 8192)
			if _, err := io.ReadFull(response.Body, prefix); err != nil {
				t.Fatal(err)
			}
			release()
			rest, err := io.ReadAll(response.Body)
			if err == nil || int64(len(prefix)+len(rest)) >= response.ContentLength {
				t.Fatalf("corrupt body completed: %d bytes, %v", len(rest), err)
			}
			if entries := cache.Inventory(nil); len(entries) != 0 {
				t.Fatalf("cached corrupt archive: %v", entries)
			}
		})
	}
}
