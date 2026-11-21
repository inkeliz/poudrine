package poudrine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func brokerRequest(t *testing.T, k ed25519.PrivateKey, method, path string, body []byte) *http.Request {
	t.Helper()
	return signedTestRequest(t, k, method, path, body, time.Now())
}

func TestSupervisorSettingsChannel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poudrine.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	const token = "1234567890123456789012345678901234567890123"
	if err := os.WriteFile(path+".setup-token", []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	parent, web, err := configBrokerPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { web.Close() })
	a := &configAuthority{path: path}
	go a.serve(parent)
	c := newConfigClient(web)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	bad := []byte(`{"token":"other"}`)
	if _, status, _ := c.submit(brokerRequest(t, other, "POST", "/api/setup", bad), bad); status != 403 {
		t.Fatal(status)
	}
	payload, _ := json.Marshal(map[string]any{"token": token})
	for _, want := range []struct {
		key    ed25519.PrivateKey
		status int
	}{{key, 204}, {key, 204}, {other, 409}} {
		if _, status, err := c.submit(brokerRequest(t, want.key, "POST", "/api/setup", payload), payload); status != want.status {
			t.Fatalf("enroll: %d/%v", status, err)
		}
	}
	queryBody := []byte("{ }\r\n")
	req := brokerRequest(t, key, methodQuery, "/api/admin/config", queryBody)
	if reply, err := c.read(req, []byte("{}")); err == nil || reply.Status != http.StatusUnauthorized {
		t.Fatalf("supervisor accepted changed QUERY bytes: %d/%v", reply.Status, err)
	}
	badQuery := []byte(`{"path":"/"}`)
	if reply, err := c.read(brokerRequest(t, key, methodQuery, "/api/admin/config", badQuery), badQuery); err == nil || reply.Status != http.StatusBadRequest {
		t.Fatalf("supervisor accepted an unknown QUERY parameter: %d/%v", reply.Status, err)
	}
	reply, err := c.read(req, queryBody)
	if reply.Status != 200 || err != nil {
		t.Fatalf("query: %d/%v", reply.Status, err)
	}
	old, rev := *reply.Settings, reply.Revision
	old.RepoURL = "https://ports.example/repo"
	payload, _ = json.Marshal(map[string]any{"settings": old, "revision": rev})
	if _, status, _ := c.submit(brokerRequest(t, other, "PUT", "/api/admin/config", payload), payload); status != 401 {
		t.Fatal("forged config signature accepted")
	}
	first := brokerRequest(t, key, "PUT", "/api/admin/config", payload)
	nextRev, status, err := c.submit(first, payload)
	if status != 200 || err != nil {
		t.Fatalf("update: %d/%v", status, err)
	}
	if nextRev == rev {
		t.Fatal("settings revision did not change")
	}
	if _, status, _ := c.submit(first, payload); status != 409 {
		t.Fatal("accepted stale revision")
	}
	// Simulate reverting the settings on disk and repeating a captured signed
	// request after a clean supervisor restart.
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, status, err := c.submit(first, payload); status != 409 || !strings.Contains(err.Error(), "processed") {
		t.Fatalf("accepted replay: %d/%v", status, err)
	}
	old.BuildStatus = "/etc/passwd"
	payload, _ = json.Marshal(map[string]any{"settings": old, "revision": rev})
	if _, status, _ := c.submit(brokerRequest(t, key, "PUT", "/api/admin/config", payload), payload); status != 400 {
		t.Fatal("accepted system-file overwrite")
	}
}

func TestRootConfigTrust(t *testing.T) {
	if err := checkRootConfigPath("/etc/passwd"); err != nil {
		t.Fatal(err)
	}
	if err := checkRootConfigPath("/tmp/poudrine-unowned-config.json"); err == nil {
		t.Fatal("trusted missing file below world writable directory")
	}
}

func TestWorkerPathsFromWebSettings(t *testing.T) {
	old := defaultConfiguration()
	path := "/usr/local/etc/poudrine.json"
	for _, dest := range []string{"/mnt/poudrine-test/worker", "/media/storage/poudrine"} {
		c := old
		c.BaseFS = dest
		if err := checkBrokerSettings(path, old, c); err != nil {
			t.Fatalf("%s: %v", dest, err)
		}
	}
	for _, change := range []func(*Configuration){
		func(c *Configuration) { c.BuildStatus = "/mnt/poudrine-test/hook.sh" },
		func(c *Configuration) { c.PackagesFile = "/etc/passwd" },
		func(c *Configuration) { c.State = "/etc/rc.d/new" },
		func(c *Configuration) { c.BuildStatus = filepath.Join(c.BaseFS, "jails", "status.json") },
		func(c *Configuration) { c.ServiceUser = "root" },
	} {
		c := old
		change(&c)
		if err := checkBrokerSettings(path, old, c); err == nil {
			t.Fatalf("accepted unsafe settings %+v", c)
		}
	}
}
