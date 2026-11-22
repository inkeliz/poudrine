package broker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/routes"
)

func TestSupervisorSettingsChannel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poudrine.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	const token = "1234567890123456789012345678901234567890123"
	if err := os.WriteFile(path+config.SetupTokenSuffix, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	parent, web, err := Pair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { web.Close() })
	a := &Authority{Path: path, Builder: &builder.Builder{}}
	go a.Serve(t.Context(), parent)
	c := NewClient(web)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	bad := []byte(`{"token":"other"}`)
	if _, status, _ := c.Submit(brokerRequest(t, other, routes.Enroll.Method, routes.Enroll.Path, bad), bad); status != 403 {
		t.Fatal(status)
	}
	payload, _ := json.Marshal(map[string]any{"token": token})
	for _, want := range []struct {
		key    ed25519.PrivateKey
		status int
	}{{key, 204}, {key, 204}, {other, 409}} {
		if _, status, err := c.Submit(brokerRequest(t, want.key, routes.Enroll.Method, routes.Enroll.Path, payload), payload); status != want.status {
			t.Fatalf("enroll: %d/%v", status, err)
		}
	}
	queryBody := []byte("{ }\r\n")
	req := brokerRequest(t, key, routes.ReadConfig.Method, routes.ReadConfig.Path, queryBody)
	if reply, err := c.Read(req, []byte("{}")); err == nil || reply.Status != http.StatusUnauthorized {
		t.Fatalf("supervisor accepted changed QUERY bytes: %d/%v", reply.Status, err)
	}
	badQuery := []byte(`{"path":"/"}`)
	if reply, err := c.Read(brokerRequest(t, key, routes.ReadConfig.Method, routes.ReadConfig.Path, badQuery), badQuery); err == nil || reply.Status != http.StatusBadRequest {
		t.Fatalf("supervisor accepted an unknown QUERY parameter: %d/%v", reply.Status, err)
	}
	reply, err := c.Read(req, queryBody)
	if reply.Status != 200 || err != nil {
		t.Fatalf("query: %d/%v", reply.Status, err)
	}
	old, rev := *reply.Settings, reply.Revision
	old.RepoURL = "https://ports.example/repo"
	old.PackageOverlays = config.PackageOverlays{"databases/mydumper": {DefaultVersions: map[string]string{"mysql": "10.11m"}}}
	payload, _ = json.Marshal(map[string]any{"settings": old, "revision": rev})
	if _, status, _ := c.Submit(brokerRequest(t, other, routes.SaveConfig.Method, routes.SaveConfig.Path, payload), payload); status != 401 {
		t.Fatal("forged config signature accepted")
	}
	first := brokerRequest(t, key, routes.SaveConfig.Method, routes.SaveConfig.Path, payload)
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	reply, err = c.Exchange(ForwardedConfigRequest(first.WithContext(expired), payload))
	if err != nil || reply.Status != http.StatusServiceUnavailable || !strings.Contains(reply.Error, "deadline exceeded") {
		t.Fatalf("expired preflight: %+v/%v", reply, err)
	}
	// Retrying the same signed request must still succeed; the expired
	// request did not advance the revision or consume its nonce.
	nextRev, status, err := c.Submit(first, payload)
	if status != 200 || err != nil {
		t.Fatalf("update: %d/%v", status, err)
	}
	if nextRev == rev {
		t.Fatal("settings revision did not change")
	}
	reply, err = c.Read(brokerRequest(t, key, routes.ReadConfig.Method, routes.ReadConfig.Path, queryBody), queryBody)
	if err != nil || !reply.WorkerPending || !reply.Settings.PackageOverlays.Equal(old.PackageOverlays) {
		t.Fatalf("expected saved overlay pending worker reload: %+v, %v", reply, err)
	}
	if _, status, _ := c.Submit(first, payload); status != 409 {
		t.Fatal("accepted stale revision")
	}
	// Simulate reverting the settings on disk and repeating a captured signed
	// request after a clean supervisor restart.
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, status, err := c.Submit(first, payload); status != 409 || !strings.Contains(err.Error(), "processed") {
		t.Fatalf("accepted replay: %d/%v", status, err)
	}
	old.BuildStatus = "/etc/passwd"
	payload, _ = json.Marshal(map[string]any{"settings": old, "revision": rev})
	if _, status, _ := c.Submit(brokerRequest(t, key, routes.SaveConfig.Method, routes.SaveConfig.Path, payload), payload); status != 400 {
		t.Fatal("accepted system-file overwrite")
	}
}
