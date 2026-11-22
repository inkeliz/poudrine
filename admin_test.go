package poudrine

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const testRepositoryPublicKey = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAxO3nFracWdZGsp07HTdd
7pH8+4qRKy5AMsf3618fzgW4uEH3HB5+6J2+kIE2Wgt4J/0/p+nGcgtuexI7m2w9
P+I/bDgRmb9dkzWTJEQn96jyHm4JMzIHLvg5kHFb6+hAzkdlzBIuMjkm9ojpV7aA
8J3+Vs8NfCM1ueKcq9CKQs4WF9W+Np5BgRmMNkJkMGYmDYeRktVLZe+zBh5FsuNM
RqbRh9vJj+EF6T5DSHyvPjW+j4tfWBIbuDKuXh28RLWzUfslvOUeLEQLU4mqb1qw
f7Zw6RSrb4mLc8XENzjLz8KHnolRqPbxRGvNUmZNEzr7mn8UraVr7nHvctHP2dn1
eQIDAQAB
-----END PUBLIC KEY-----
`

func testAdmin(t *testing.T) *Admin {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "admin.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	private := func(context.Context) (any, error) { return []string{"private-client.example"}, nil }
	a, err := NewAdmin(AdminConfig{DB: db, DataDir: dir, RepositoryURL: "https://repo.example", PublicKey: func() string { return testRepositoryPublicKey }, Stats: func() CatalogStats { return CatalogStats{Packages: 12, Ahead: 2} }, Clients: private, Tracked: private, Build: func() (BuildStatus, error) { return BuildStatus{Stage: "waiting"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newTestAdminKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Attach a supervisor to an enrolled installation. The live descriptor pair
// exercises both signature checks and the privileged on-disk identity.
func supervisedTestAdmin(t *testing.T, cfg AdminConfig, key ed25519.PrivateKey, b *Builder) (*Admin, *configAuthority) {
	t.Helper()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "poudrine.json")
	if err := os.WriteFile(cfg.ConfigPath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.ConfigPath+".admin.pub", key.Public().(ed25519.PublicKey), 0600); err != nil {
		t.Fatal(err)
	}
	parent, child, err := configBrokerPair()
	if err != nil {
		t.Fatal(err)
	}
	authority := &configAuthority{path: cfg.ConfigPath, builder: b}
	done := make(chan struct{})
	go func() {
		authority.serve(parent)
		close(done)
	}()
	t.Cleanup(func() {
		child.Close()
		<-done
	})
	cfg.broker = newConfigClient(child)
	cfg.pinnedKey = key.Public().(ed25519.PublicKey)
	admin, err := NewAdmin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return admin, authority
}

func signedTestRequest(t *testing.T, key ed25519.PrivateKey, method, path string, body []byte, ts time.Time) *http.Request {
	t.Helper()
	var raw [16]byte
	rand.Read(raw[:])
	nonce := base64.RawURLEncoding.EncodeToString(raw[:])
	stamp := fmt.Sprint(ts.Unix())
	prefix := fmt.Sprintf("poudrine-v2\n%s\n%s\n%s\n%s\n", method, path, stamp, nonce)
	msg := append([]byte(prefix), body...)
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("X-Poudrine-Key", base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
	r.Header.Set("X-Poudrine-Time", stamp)
	r.Header.Set("X-Poudrine-Nonce", nonce)
	r.Header.Set("X-Poudrine-Signature", base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, msg)))
	return r
}

func adminResponse(t *testing.T, a *Admin, r *http.Request, want int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s got %d wanted %d (%s)", r.Method, r.URL, w.Code, want, w.Body.String())
	}
	return w
}

func enrollAdmin(t *testing.T, a *Admin) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(a.tokenPath())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": strings.TrimSpace(string(token))})
	r := signedTestRequest(t, priv, "POST", "/api/setup", body, time.Now())
	r.Header.Set("X-Poudrine-Key", base64.RawURLEncoding.EncodeToString(pub))
	adminResponse(t, a, r, 204)
	adminResponse(t, a, signedTestRequest(t, priv, "POST", "/api/setup", body, time.Now()), 409)
	return priv
}

func TestAdminEnrollmentAndSignedRequests(t *testing.T) {
	a := testAdmin(t)
	for _, path := range []string{"/", "/admin", "/repository.pub", "/poudrine.js"} {
		w := adminResponse(t, a, httptest.NewRequest("GET", path, nil), 200)
		if strings.Contains(w.Body.String(), "private-client.example") {
			t.Fatal("public data leak")
		}
	}
	adminResponse(t, a, httptest.NewRequest(methodQuery, "/api/admin/clients", nil), 401)
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	adminResponse(t, a, signedTestRequest(t, other, "POST", "/api/setup", []byte(`{"token":"bad"}`), time.Now()), 403)
	key := enrollAdmin(t, a)
	path := "/api/admin/clients"
	adminResponse(t, a, signedTestRequest(t, other, methodQuery, path, []byte("{}"), time.Now()), 401)
	adminResponse(t, a, signedTestRequest(t, key, methodQuery, path, []byte("{}"), time.Now().Add(-6*time.Minute)), 401)
	tampered := signedTestRequest(t, key, methodQuery, path, []byte("{}"), time.Now())
	tampered.URL.RawQuery = "limit=500"
	adminResponse(t, a, tampered, 400)
	r := signedTestRequest(t, key, methodQuery, path, []byte("{}"), time.Now())
	if !strings.Contains(adminResponse(t, a, r, 200).Body.String(), "private-client.example") {
		t.Fatal("client missing")
	}
	restarted, err := NewAdmin(a.cfg)
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
	otherKey := newTestAdminKey(t)
	for _, path := range []string{
		"/api/status", "/api/setup", "/api/admin/config", buildStatusPath, buildEventsPath,
		originsPath, packagesPath, clientSummaryPath, clientPackagesPath, buildStatusPath + "/history",
		"/api/admin/clients", "/api/admin/tracked", "/api/admin/push-key", "/api/admin/build/issues", "/api/admin/build/results", "/api/admin/build/log",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := adminResponse(t, a, httptest.NewRequest(method, path, nil), http.StatusMethodNotAllowed)
			if !strings.Contains(w.Header().Get("Allow"), methodQuery) {
				t.Fatalf("missing QUERY method for %s", path)
			}
		}
		adminResponse(t, a, httptest.NewRequest(methodQuery, path, strings.NewReader("{}")), http.StatusUnauthorized)
		adminResponse(t, a, signedTestRequest(t, otherKey, methodQuery, path, []byte("{}"), time.Now()), http.StatusUnauthorized)

		modified := signedTestRequest(t, key, methodQuery, path, []byte("{}"), time.Now())
		modified.Body = io.NopCloser(strings.NewReader("{ }"))
		adminResponse(t, a, modified, http.StatusUnauthorized)
	}

	for _, path := range []string{"/api/admin/tracked", "/api/admin/clients", "/api/admin/push-key"} {
		req := signedTestRequest(t, key, methodQuery, path, []byte("{}"), time.Now())
		req.Header.Del("X-Poudrine-Key") // Management obtains the enrolled key from the server.
		adminResponse(t, a, req, http.StatusOK)
		replay := httptest.NewRequest(methodQuery, path, strings.NewReader("{}"))
		replay.Header = req.Header.Clone()
		adminResponse(t, a, replay, http.StatusConflict)
	}

	for _, body := range []string{"", "[]", "null", "{}{}", "{", "{}\v", `{"unknown":0}`} {
		req := signedTestRequest(t, key, methodQuery, "/api/status", []byte(body), time.Now())
		adminResponse(t, a, req, http.StatusBadRequest)
	}
	for _, suffix := range []string{"?", "?ip=::1"} {
		for _, op := range []struct{ method, path string }{
			{methodQuery, "/api/status"}, {http.MethodPost, "/api/setup"}, {http.MethodPost, "/api/admin/login"},
			{http.MethodPut, "/api/admin/config"}, {http.MethodDelete, "/api/admin/subscription"},
		} {
			req := signedTestRequest(t, key, op.method, op.path+suffix, []byte("{}"), time.Now())
			adminResponse(t, a, req, http.StatusBadRequest)
		}
	}
	history := signedTestRequest(t, key, methodQuery, buildStatusPath+"/history", []byte(`{"interval":"day"}`), time.Now())
	history.Body = io.NopCloser(strings.NewReader(`{"interval":"week"}`))
	adminResponse(t, a, history, http.StatusUnauthorized)
	w := adminResponse(t, a, signedTestRequest(t, key, methodQuery, "/api/status", []byte(" { }\r\n"), time.Now()), http.StatusOK)
	var stats CatalogStats
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
	path := "/api/admin/subscription"
	tampered := signedTestRequest(t, key, "POST", path, body, time.Now())
	tampered.Body = io.NopCloser(strings.NewReader("{}"))
	adminResponse(t, a, tampered, 401)
	adminResponse(t, a, signedTestRequest(t, key, "POST", path, body, time.Now()), 204)
	now := time.Now()
	record := BuildRecord{Package: "test-2", Started: now.Add(-time.Minute), Finished: now, Outcome: "succeeded", Origin: "devel/test", ABI: "freebsd:15:amd64"}
	if err := (inventory{a.cfg.DB}).recordBuilds(t.Context(), []BuildRecord{record}); err != nil {
		t.Fatal(err)
	}
	snapshot := &catalogSnapshot{displaced: map[string]CatalogPackage{"test": {Name: "test", Origin: "devel/test", Version: "1", ABI: "freebsd:15:amd64"}}}
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
