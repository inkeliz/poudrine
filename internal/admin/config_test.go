package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	configpkg "github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/routes"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestAutomaticPushContactIP(t *testing.T) {
	admin := testAdmin(t)
	admin.cfg.RepositoryURL = "http://[::1]:8080"
	if err := admin.initPush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if admin.pushContact != "postmaster@%5BIPv6:::1%5D" {
		t.Fatalf("invalid automatic IPv6 mailto contact %q", admin.pushContact)
	}
}

func TestWebRejectsInvalidConfigBeforeWriting(t *testing.T) {
	for _, mode := range []string{"standalone", "supervised"} {
		t.Run(mode, func(t *testing.T) {
			a := testAdmin(t)
			key := enrollAdmin(t, a)
			if mode == "supervised" {
				a, _ = supervisedTestAdmin(t, a.cfg, key, nil)
			} else {
				a.cfg.ConfigPath = filepath.Join(t.TempDir(), "config.json")
			}
			path := a.cfg.ConfigPath
			prior := []byte("{\n \"jobs\": 2\n}\n")
			if err := os.WriteFile(path, prior, 0600); err != nil {
				t.Fatal(err)
			}
			for _, raw := range []string{`{"jobs":4,"jobs":5}`, `{"web_tls":null}`, `{"refresh":"tomorrow"}`, `{"tls_cert":"missing","tls_key":"key"}`,
				`{"package_overlays":{"devel/test":{"options_set":["DOCS\nEXTRA"]}}}`,
				`{"package_overlays":{"devel/test":{"default_versions":{"mysql":"8","mysql":"10.11m"}}}}`} {
				b, err := json.Marshal(configUpdate{Settings: json.RawMessage(raw), Revision: configpkg.ConfigRevision(prior)})
				if err != nil {
					t.Fatal(err)
				}
				req := testutil.SignedRequest(t, key, http.MethodPut, routes.ReadConfig.Path, b, time.Now())
				adminResponse(t, a, req, http.StatusBadRequest)
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, prior) {
					t.Fatalf("invalid settings changed %s: %v", path, err)
				}
			}
			if err := os.WriteFile(path, []byte(`{"jobs":"six"}`), 0600); err != nil {
				t.Fatal(err)
			}
			get := testutil.SignedRequest(t, key, routes.ReadConfig.Method, routes.ReadConfig.Path, []byte("{}"), time.Now())
			w := adminResponse(t, a, get, http.StatusUnprocessableEntity)
			if !strings.Contains(w.Body.String(), "jobs:") {
				t.Fatalf("manual edit diagnostic: %s", w.Body.String())
			}
		})
	}
}

func TestWebSettingsPrefillPreservesAutomaticConfiguration(t *testing.T) {
	a := testAdmin(t)
	key := enrollAdmin(t, a)
	dir := t.TempDir()
	a.cfg.ConfigPath = filepath.Join(dir, "poudrine.json")
	a.cfg.RepositoryPort = 49123
	a.cfg.UpstreamURL = "https://pkg.freebsd.org/FreeBSD:15:amd64/latest"
	config := []byte(`{"repo_listen":"[::1]:0","state":"private","jail":"builder","ports":"quarterly","cache_gb":0}`)
	if err := os.WriteFile(a.cfg.ConfigPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	req := testutil.SignedRequest(t, key, routes.ReadConfig.Method, routes.ReadConfig.Path, []byte("{}"), time.Now())
	reply := adminResponse(t, a, req, http.StatusOK)
	var out struct {
		Settings     configpkg.Configuration
		Defaults     map[string]string
		ServerTimeAt time.Time `json:"server_time_at"`
	}
	if err := json.Unmarshal(reply.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ServerTimeAt.IsZero() {
		t.Fatal("working hours need the server clock and UTC offset")
	}
	want := map[string]string{
		"repo_url": "http://[::1]:49123", "push_contact": "postmaster@[IPv6:::1]",
		"local_repo": filepath.Join(out.Settings.BaseFS, "data/packages/builder-quarterly"),
		"upstream":   a.cfg.UpstreamURL, "working_hours": "00:00-24:00",
		"tls_cert": filepath.Join(dir, "private/web-tls.crt"), "tls_key": filepath.Join(dir, "private/web-tls.key"),
	}
	for field, value := range want {
		if out.Defaults[field] != value {
			t.Errorf("%s prefill = %q, want %q", field, out.Defaults[field], value)
		}
	}
	raw, _, err := configpkg.LoadConfiguration(a.cfg.ConfigPath)
	if err != nil || !reflect.DeepEqual(raw, out.Settings) || raw.RepoURL != "" || raw.TLSCert != "" || raw.WorkingHours != "" || raw.CacheGB != 0 {
		t.Fatalf("configuration and automatic defaults diverged: %+v, %v", out.Settings, err)
	}
}
