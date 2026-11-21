package poudrine

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
	const login = "/api/admin/login"
	body := []byte("{}")
	_, wrongKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	adminResponse(t, admin, signedTestRequest(t, wrongKey, "POST", login, body, time.Now()), 401)
	adminResponse(t, admin, signedTestRequest(t, wrongKey, methodQuery, "/api/setup", body, time.Now()), 401)
	key := enrollAdmin(t, admin)
	w := adminResponse(t, admin, signedTestRequest(t, key, methodQuery, "/api/setup", body, time.Now()), 200)
	if strings.TrimSpace(w.Body.String()) != `{"enrolled":true}` {
		t.Fatal("enrollment status did not reflect enrolled administrator")
	}
	checkPage(admin, true)
	restarted, err := NewAdmin(admin.cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkPage(restarted, true)
	adminResponse(t, restarted, signedTestRequest(t, wrongKey, "POST", login, body, time.Now()), 401)
	req := signedTestRequest(t, key, "POST", login, body, time.Now())
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
	other := newTestAdminKey(t)
	body, err := json.Marshal(map[string]string{"public_key": base64.RawURLEncoding.EncodeToString(other.Public().(ed25519.PublicKey))})
	if err != nil {
		t.Fatal(err)
	}
	req := signedTestRequest(t, current, http.MethodPost, path, body, time.Now())
	adminResponse(t, a, req, http.StatusNotFound)

	a, authority := supervisedTestAdmin(t, a.cfg, current, nil)
	req = signedTestRequest(t, current, http.MethodPost, path, body, time.Now())
	adminResponse(t, a, req, http.StatusNotFound)
	if _, status, err := a.cfg.broker.submit(req, body); err == nil || status < 400 {
		t.Fatalf("supervisor accepted an unsupported request: %d %v", status, err)
	}
	pin, err := authority.publicKey()
	if err != nil || !pin.Equal(current.Public()) {
		t.Fatalf("enrolled supervisor key changed: %v", err)
	}
}
