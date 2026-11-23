package catalog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestCacheConcurrentDistinctAndCoalescedDownloads(t *testing.T) {
	for _, limit := range []int64{0, 1024} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			entered := make(chan string, 10)
			finish := make(chan struct{})
			var hits, active, peak atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				entered <- r.URL.Path
				<-finish
				fmt.Fprint(w, r.URL.Path)
			}))
			defer up.Close()
			var once sync.Once
			release := func() { once.Do(func() { close(finish) }) }
			defer release()
			u, _ := url.Parse(up.URL)
			cache, err := NewCache(t.TempDir(), limit, u, up.Client())
			if err != nil {
				t.Fatal(err)
			}
			requests := make([]packageinfo.Package, 5)
			for i := range requests {
				name := fmt.Sprint(i)
				requests[i] = testutil.Package(name, "/All/"+name+".pkg")
			}
			result := make(chan error, 6)
			open := func(p packageinfo.Package) {
				f, err := cache.Open(t.Context(), p)
				if err == nil {
					defer f.Close()
					var body []byte
					body, err = io.ReadAll(f)
					if err == nil && string(body) != "/"+p.Path {
						err = fmt.Errorf("wrong body %q", body)
					}
				}
				result <- err
			}
			go open(requests[0])
			<-entered
			go open(requests[0])
			// Wait until the duplicate has joined the first flight, including when
			// caching is disabled and no completed cache entry can hide a duplicate.
			deadline := time.Now().Add(time.Second)
			for {
				cache.mu.Lock()
				users := cache.downloads[PackageCacheKey(requests[0].Sum)].users
				cache.mu.Unlock()
				if users == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("duplicate did not coalesce")
				}
				time.Sleep(time.Millisecond)
			}
			for _, p := range requests[1:] {
				go open(p)
			}
			for range 3 {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("distinct downloads serialized")
				}
			}
			select {
			case <-entered:
				t.Fatal("more than four upstream downloads")
			case <-time.After(20 * time.Millisecond):
			}
			release()
			for range 6 {
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			}
			if hits.Load() != 5 || peak.Load() > 4 {
				t.Fatalf("hits %d peak %d", hits.Load(), peak.Load())
			}
		})
	}
}

func TestCacheCanceledWaiterDoesNotCancelOwner(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-finish; fmt.Fprint(w, "archive") }))
	defer up.Close()
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	defer release()
	u, _ := url.Parse(up.URL)
	cache, err := NewCache(t.TempDir(), 1024, u, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	p := testutil.Package("pkg", "archive")
	owner := make(chan error, 1)
	go func() {
		f, err := cache.Open(t.Context(), p)
		if f != nil {
			f.Close()
		}
		owner <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiter := make(chan error, 1)
	go func() {
		f, err := cache.Open(ctx, p)
		if f != nil {
			f.Close()
		}
		waiter <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		cache.mu.Lock()
		users := cache.downloads[PackageCacheKey(p.Sum)].users
		cache.mu.Unlock()
		if users == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter did not join")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-waiter; err != context.Canceled {
		t.Fatalf("canceled waiter: %v", err)
	}
	release()
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
}
