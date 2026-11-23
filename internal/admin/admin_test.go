package admin

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestAdminEnrollmentAndSignedRequests(t *testing.T) {
	a := testAdmin(t)
	for _, path := range []string{"/", "/admin", "/repository.pub", "/poudrine.js", "/fonts/sora-latin.woff2", "/fonts/sora-latin-ext.woff2", "/fonts/atkinson-hyperlegible-next.woff2", "/fonts/geist-mono.woff2"} {
		w := adminResponse(t, a, httptest.NewRequest("GET", path, nil), 200)
		if strings.Contains(w.Body.String(), "private-client.example") {
			t.Fatal("public data leak")
		}
	}
	adminResponse(t, a, httptest.NewRequest(routes.Clients.Method, routes.Clients.Path, nil), 401)
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	adminResponse(t, a, testutil.SignedRequest(t, other, routes.Enroll.Method, routes.Enroll.Path, []byte(`{"token":"bad"}`), time.Now()), 403)
	key := enrollAdmin(t, a)
	path := routes.Clients.Path
	adminResponse(t, a, testutil.SignedRequest(t, other, broker.MethodQuery, path, []byte("{}"), time.Now()), 401)
	adminResponse(t, a, testutil.SignedRequest(t, key, broker.MethodQuery, path, []byte("{}"), time.Now().Add(-6*time.Minute)), 401)
	tampered := testutil.SignedRequest(t, key, broker.MethodQuery, path, []byte("{}"), time.Now())
	tampered.URL.RawQuery = "limit=500"
	adminResponse(t, a, tampered, 400)
	r := testutil.SignedRequest(t, key, broker.MethodQuery, path, []byte("{}"), time.Now())
	if !strings.Contains(adminResponse(t, a, r, 200).Body.String(), "private-client.example") {
		t.Fatal("client missing")
	}
	restarted, err := New(t.Context(), a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Body = io.NopCloser(strings.NewReader("{}"))
	adminResponse(t, restarted, r, 409)
	if restarted.pushPublic != a.pushPublic {
		t.Fatal("VAPID identity rotated across restart")
	}
	if _, err := os.Stat(a.tokenPath()); !os.IsNotExist(err) {
		t.Fatal("setup token was not consumed")
	}
}

func TestManagementQueryProtocol(t *testing.T) {
	a := testAdmin(t)
	key := enrollAdmin(t, a)
	otherKey := testutil.AdminKey(t)
	for _, path := range []string{
		routes.Status.Path, routes.Setup.Path, routes.ReadConfig.Path, routes.BuildStatus.Path, routes.BuildEvents.Path,
		routes.Origins.Path, routes.Packages.Path, routes.ClientSummary.Path, routes.ClientPackages.Path, routes.BuildHistory.Path, routes.HistoryPackages.Path,
		routes.Clients.Path, routes.Tracked.Path, routes.PushKey.Path, routes.BuildIssues.Path, routes.BuildResults.Path, routes.BuildLog.Path,
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := adminResponse(t, a, httptest.NewRequest(method, path, nil), http.StatusMethodNotAllowed)
			if !strings.Contains(w.Header().Get("Allow"), broker.MethodQuery) {
				t.Fatalf("missing QUERY method for %s", path)
			}
		}
		adminResponse(t, a, httptest.NewRequest(broker.MethodQuery, path, strings.NewReader("{}")), http.StatusUnauthorized)
		adminResponse(t, a, testutil.SignedRequest(t, otherKey, broker.MethodQuery, path, []byte("{}"), time.Now()), http.StatusUnauthorized)

		modified := testutil.SignedRequest(t, key, broker.MethodQuery, path, []byte("{}"), time.Now())
		modified.Body = io.NopCloser(strings.NewReader("{ }"))
		adminResponse(t, a, modified, http.StatusUnauthorized)
	}

	for _, path := range []string{routes.Tracked.Path, routes.Clients.Path, routes.PushKey.Path} {
		req := testutil.SignedRequest(t, key, broker.MethodQuery, path, []byte("{}"), time.Now())
		req.Header.Del("X-Poudrine-Key") // Management obtains the enrolled key from the server.
		adminResponse(t, a, req, http.StatusOK)
		replay := httptest.NewRequest(broker.MethodQuery, path, strings.NewReader("{}"))
		replay.Header = req.Header.Clone()
		adminResponse(t, a, replay, http.StatusConflict)
	}

	for _, body := range []string{"", "[]", "null", "{}{}", "{", "{}\v", `{"unknown":0}`} {
		req := testutil.SignedRequest(t, key, routes.Status.Method, routes.Status.Path, []byte(body), time.Now())
		adminResponse(t, a, req, http.StatusBadRequest)
	}
	for _, suffix := range []string{"?", "?ip=::1"} {
		for _, op := range []struct{ method, path string }{
			{routes.Status.Method, routes.Status.Path}, {http.MethodPost, routes.Setup.Path}, {routes.Login.Method, routes.Login.Path},
			{http.MethodPut, routes.ReadConfig.Path}, {http.MethodDelete, routes.Subscribe.Path},
		} {
			req := testutil.SignedRequest(t, key, op.method, op.path+suffix, []byte("{}"), time.Now())
			adminResponse(t, a, req, http.StatusBadRequest)
		}
	}
	history := testutil.SignedRequest(t, key, routes.BuildHistory.Method, routes.BuildHistory.Path, []byte(`{"interval":"day"}`), time.Now())
	history.Body = io.NopCloser(strings.NewReader(`{"interval":"week"}`))
	adminResponse(t, a, history, http.StatusUnauthorized)
	w := adminResponse(t, a, testutil.SignedRequest(t, key, routes.Status.Method, routes.Status.Path, []byte(" { }\r\n"), time.Now()), http.StatusOK)
	var stats catalog.CatalogStats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil || stats.Packages != 12 || stats.Ahead != 2 {
		t.Fatalf("unexpected signed catalog status: %s (%v)", w.Body, err)
	}
}

func TestSharedWebScript(t *testing.T) {
	a := testAdmin(t)
	get := func(path string, status int) *httptest.ResponseRecorder {
		return adminResponse(t, a, httptest.NewRequest(http.MethodGet, path, nil), status)
	}
	for _, page := range []string{"/", "/admin"} {
		body := get(page, http.StatusOK).Body.String()
		if strings.Count(body, "<script ") != 1 || !strings.Contains(body, `src="poudrine.js"`) {
			t.Fatalf("%s must load the shared script once", page)
		}
	}
	script := get("/poudrine.js", http.StatusOK)
	for _, url := range []string{"/sw.js", "/status.js", "/admin.js"} {
		r := get(url, http.StatusOK)
		if !bytes.Equal(r.Body.Bytes(), script.Body.Bytes()) || !strings.Contains(r.Header().Get("Content-Type"), "javascript") {
			t.Fatalf("expected shared JavaScript at %s", url)
		}
	}
	for _, url := range []string{"/builds.js", "/inventory.js"} {
		get(url, http.StatusNoContent)
	}
}

func TestAdminWebPushDelivery(t *testing.T) {
	a := testAdmin(t)
	key := enrollAdmin(t, a)
	device, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var secret [16]byte
	rand.Read(secret[:])
	received := false
	push := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		if err != nil || len(b) != 4096 || bytes.Contains(b, []byte("test-2")) || r.Header.Get("Topic") == "" || r.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") {
			t.Error("malformed encrypted push")
		}
		received = true
		w.WriteHeader(http.StatusGone)
	}))
	defer push.Close()
	a.pushClient = push.Client()
	sub := map[string]any{"endpoint": push.URL, "keys": map[string]string{"auth": base64.RawURLEncoding.EncodeToString(secret[:]), "p256dh": base64.RawURLEncoding.EncodeToString(device.PublicKey().Bytes())}}
	body, err := json.Marshal(sub)
	if err != nil {
		t.Fatal(err)
	}
	path := routes.Subscribe.Path
	tampered := testutil.SignedRequest(t, key, "POST", path, body, time.Now())
	tampered.Body = io.NopCloser(strings.NewReader("{}"))
	adminResponse(t, a, tampered, 401)
	subscribe := testutil.SignedRequest(t, key, "POST", path, body, time.Now())
	subscribe.Header.Set("Accept-Language", "en;q=0.2, de-CH")
	adminResponse(t, a, subscribe, 204)
	now := time.Now()
	record := database.BuildRecord{Package: "test-2", Started: now.Add(-time.Minute), Finished: now, Outcome: "succeeded", Origin: "devel/test", ABI: "freebsd:15:amd64"}
	if err := a.cfg.DB.RecordBuilds(t.Context(), []database.BuildRecord{record}); err != nil {
		t.Fatal(err)
	}
	batch, err := a.cfg.DB.PendingBuildPush(t.Context(), now)
	if err != nil || len(batch) != 1 || batch[0].Language != "de-CH" {
		t.Fatalf("queued notification language: %+v, %v", batch, err)
	}
	snapshot := &catalog.Snapshot{Displaced: map[string]packageinfo.Package{"test": {Name: "test", Origin: "devel/test", Version: "1", ABI: "freebsd:15:amd64"}}}
	if err := a.deliverBuildPush(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if !received {
		t.Fatal("missing push request")
	}
	var count int
	if err := a.cfg.DB.QueryRow("SELECT count(*) FROM push_subscriptions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired subscription count=%d err=%v", count, err)
	}
}
