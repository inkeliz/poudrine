package admin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/broker"
	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/database"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func testAdmin(t *testing.T) *Admin {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "admin.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clients := func(context.Context) ([]database.ClientRecord, error) {
		return []database.ClientRecord{{IP: "private-client.example"}}, nil
	}
	tracked := func(context.Context) ([]database.TrackedPackageRecord, error) {
		return []database.TrackedPackageRecord{{Name: "private-client.example"}}, nil
	}
	a, err := New(t.Context(), Config{DB: &database.DB{DB: db}, DataDir: dir, RepositoryURL: "https://repo.example", PublicKey: func() string { return testutil.RepositoryPublicKey }, Stats: func() catalog.CatalogStats { return catalog.CatalogStats{Packages: 12, Ahead: 2} }, Clients: clients, Tracked: tracked, Build: func() (builder.BuildStatus, error) { return builder.BuildStatus{Stage: "waiting"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Attach a supervisor to an enrolled installation. The live descriptor pair
// exercises both signature checks and the privileged on-disk identity.
func supervisedTestAdmin(t *testing.T, cfg Config, key ed25519.PrivateKey, b *builder.Builder) (*Admin, *broker.Authority) {
	t.Helper()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "poudrine.json")
	if err := os.WriteFile(cfg.ConfigPath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.ConfigPath+config.AdminKeySuffix, key.Public().(ed25519.PublicKey), 0600); err != nil {
		t.Fatal(err)
	}
	parent, child, err := broker.Pair()
	if err != nil {
		t.Fatal(err)
	}
	authority := &broker.Authority{Path: cfg.ConfigPath, Builder: b}
	done := make(chan struct{})
	go func() {
		authority.Serve(t.Context(), parent)
		close(done)
	}()
	t.Cleanup(func() {
		child.Close()
		<-done
	})
	cfg.Broker = broker.NewClient(child)
	cfg.PinnedKey = key.Public().(ed25519.PublicKey)
	admin, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return admin, authority
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
	r := testutil.SignedRequest(t, priv, routes.Enroll.Method, routes.Enroll.Path, body, time.Now())
	r.Header.Set("X-Poudrine-Key", base64.RawURLEncoding.EncodeToString(pub))
	adminResponse(t, a, r, 204)
	adminResponse(t, a, testutil.SignedRequest(t, priv, routes.Enroll.Method, routes.Enroll.Path, body, time.Now()), 409)
	return priv
}
