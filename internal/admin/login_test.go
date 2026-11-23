package admin

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestSetupStatusAndKeyOnlyLogin(t *testing.T) {
	admin := testAdmin(t)
	checkPage := func(a *Admin, want bool) {
		t.Helper()
		w := adminResponse(t, a, httptest.NewRequest("GET", "/admin", nil), 200)
		if !strings.Contains(w.Body.String(), `data-enrolled="`+strconv.FormatBool(want)+`"`) {
			t.Fatalf("Management page did not show enrollment state %t", want)
		}
	}
	checkPage(admin, false)
	login := routes.Login.Path
	body := []byte("{}")
	_, wrongKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	adminResponse(t, admin, testutil.SignedRequest(t, wrongKey, "POST", login, body, time.Now()), 401)
	adminResponse(t, admin, testutil.SignedRequest(t, wrongKey, routes.Setup.Method, routes.Setup.Path, body, time.Now()), 401)
	key := enrollAdmin(t, admin)
	w := adminResponse(t, admin, testutil.SignedRequest(t, key, routes.Setup.Method, routes.Setup.Path, body, time.Now()), 200)
	if strings.TrimSpace(w.Body.String()) != `{"enrolled":true}` {
		t.Fatal("enrollment status did not reflect enrolled administrator")
	}
	checkPage(admin, true)
	restarted, err := New(t.Context(), admin.cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkPage(restarted, true)
	adminResponse(t, restarted, testutil.SignedRequest(t, wrongKey, "POST", login, body, time.Now()), 401)
	req := testutil.SignedRequest(t, key, "POST", login, body, time.Now())
	req.Header.Del("X-Poudrine-Key")
	adminResponse(t, restarted, req, 204)
	replay := httptest.NewRequest("POST", login, bytes.NewReader(body))
	replay.Header = req.Header.Clone()
	adminResponse(t, restarted, replay, 409)
}

func TestEnrolledAdminHasNoKeyReplacementRoute(t *testing.T) {
	const path = "/api/admin/replace-key"
	a := testAdmin(t)
	current := enrollAdmin(t, a)
	other := testutil.AdminKey(t)
	body, err := json.Marshal(map[string]string{"public_key": base64.RawURLEncoding.EncodeToString(other.Public().(ed25519.PublicKey))})
	if err != nil {
		t.Fatal(err)
	}
	req := testutil.SignedRequest(t, current, http.MethodPost, path, body, time.Now())
	adminResponse(t, a, req, http.StatusNotFound)

	a, authority := supervisedTestAdmin(t, a.cfg, current, nil)
	req = testutil.SignedRequest(t, current, http.MethodPost, path, body, time.Now())
	adminResponse(t, a, req, http.StatusNotFound)
	if _, status, err := a.cfg.Broker.Submit(req, body); err == nil || status < 400 {
		t.Fatalf("supervisor accepted an unsupported request: %d %v", status, err)
	}
	pin, err := authority.PublicKey()
	if err != nil || !pin.Equal(current.Public()) {
		t.Fatalf("enrolled supervisor key changed: %v", err)
	}
}
