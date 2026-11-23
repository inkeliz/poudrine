package catalog

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestUpstreamRedirectPolicy(t *testing.T) {
	client := upstreamClient(nil)
	request := func(raw string) *http.Request {
		r, err := http.NewRequest("GET", raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, tc := range []struct {
		from, to string
		allowed  bool
	}{
		{"https://pkg.example/start", "https://pkg.example/next", true},
		{"http://pkg.example/start", "https://pkg.example/next", true},
		{"http://127.0.0.1:123/start", "http://127.0.0.1:123/next", true},
		{"https://pkg.example/start", "http://pkg.example/next", false},
		{"https://pkg.example/start", "https://pkg.example:8181/next", false},
		{"http://pkg.example/start", "http://169.254.169.254/", false},
		{"https://pkg.example/start", "https://10.0.0.1/", false},
		{"https://pkg.example/start", "https://[::1]/", false},
		{"https://pkg.example/start", "https://pkg.example.attacker.invalid/", false},
		{"https://pkg.example/start", "https://user@pkg.example/", false},
	} {
		if err := client.CheckRedirect(request(tc.to), []*http.Request{request(tc.from)}); (err == nil) != tc.allowed {
			t.Errorf("%s -> %s: %v", tc.from, tc.to, err)
		}
	}
	via := []*http.Request{request("http://pkg.example/"), request("https://pkg.example/")}
	if err := client.CheckRedirect(request("http://pkg.example/again"), via); err == nil {
		t.Fatal("allowed downgrade after upgrade")
	}
	prior := errors.New("custom rejection")
	original := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return prior }}
	if err := upstreamClient(original).CheckRedirect(request("https://pkg.example/a"), via); !errors.Is(err, prior) {
		t.Fatal("lost custom redirect policy")
	}
}

func TestCacheDoesNotFollowRedirectToAnotherService(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte("secret")) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	upstream, _ := url.Parse(source.URL)
	cache, err := NewCache(t.TempDir(), 1024, upstream, source.Client())
	if err != nil {
		t.Fatal(err)
	}
	f, err := cache.Open(t.Context(), testutil.Package("test", "secret"))
	if err == nil {
		f.Close()
		t.Fatal("followed redirect to another service")
	}
	if hits.Load() != 0 {
		t.Fatal("redirect target was contacted before rejection")
	}
}
