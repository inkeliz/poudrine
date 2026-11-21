package poudrine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestConfigDocumentValidation(t *testing.T) {
	valid := []string{
		`{}`, `{"web_tls":false,"cache_gb":0.0e100}`, `{"cache_gb":0.000000001}`,
		`{"state":"./data-\ud83c\udf35"}`, `{"state":"./data-\ufffd\\ud800"}`,
		`{"make_jobs_number":8,"make_jobs_number_limit":4}`,
	}
	for _, raw := range valid {
		if _, err := decodeConfiguration([]byte(raw), ""); err != nil {
			t.Errorf("valid settings %s: %v", raw, err)
		}
	}
	for _, tc := range []struct{ raw, problem string }{
		{`{"jobs":2,"jobs":3}`, "jobs: duplicate"},
		{`{"jobs":2,"j\u006fbs":3}`, "jobs: duplicate"},
		{`{"JOBS":2}`, "unknown setting"},
		{`{"jobs":"2"}`, "jobs:"},
		{`{"make_jobs_number":-2}`, "make_jobs_number:"},
		{`{"make_jobs_number_limit":2147483648}`, "make_jobs_number_limit:"},
		{`{"web_tls":null}`, "web_tls:"},
		{`{"repo_url":null}`, "repo_url:"},
		{`{"cache_gb":null}`, "cache_gb:"},
		{`{"state":"data-\ud800"}`, "state: invalid Unicode"},
		{`{"state":"\udc00\ud83c\udf35"}`, "state: invalid Unicode"},
		{`{"state":"` + string([]byte{0xff}) + `"}`, "UTF-8"},
		{`{ "jobs":4 }{}`, "trailing data"},
		{`{"cache_gb":1e-10}`, "cache_gb:"},
		{`{"cache_gb":1e-400}`, "cache_gb:"},
		{`{"cache_gb":1e30}`, "cache_gb:"},
	} {
		_, err := decodeConfiguration([]byte(tc.raw), "")
		if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), tc.problem) {
			t.Errorf("validating %q: want %q, got %v", tc.raw, tc.problem, err)
		}
	}
}

func TestCheckConfigIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poudrine.json")
	check := func() error { return Run(context.Background(), []string{"check-config", "-config", path}) }
	if err := check(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checking missing config: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"state":"./state","tls_cert":"missing.cert","tls_key":"missing.key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil || !strings.HasPrefix(err.Error(), "tls_cert:") {
		t.Fatalf("checking missing custom certificate: %v", err)
	}
	data := []byte(`{ "state":"./state" }`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("unexpected files after check: %v %v", files, err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatal("checking valid settings changed the source file", err)
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
			for _, raw := range []string{`{"jobs":4,"jobs":5}`, `{"web_tls":null}`, `{"refresh":"tomorrow"}`, `{"tls_cert":"missing","tls_key":"key"}`} {
				b, err := json.Marshal(configUpdate{Settings: json.RawMessage(raw), Revision: configRevision(prior)})
				if err != nil {
					t.Fatal(err)
				}
				req := signedTestRequest(t, key, http.MethodPut, "/api/admin/config", b, time.Now())
				adminResponse(t, a, req, http.StatusBadRequest)
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, prior) {
					t.Fatalf("invalid settings changed %s: %v", path, err)
				}
			}
			if err := os.WriteFile(path, []byte(`{"jobs":"six"}`), 0600); err != nil {
				t.Fatal(err)
			}
			get := signedTestRequest(t, key, methodQuery, "/api/admin/config", []byte("{}"), time.Now())
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
	req := signedTestRequest(t, key, methodQuery, "/api/admin/config", []byte("{}"), time.Now())
	reply := adminResponse(t, a, req, http.StatusOK)
	var out struct {
		Settings Configuration
		Defaults map[string]string
	}
	if err := json.Unmarshal(reply.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
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
	raw, _, err := loadConfiguration(a.cfg.ConfigPath)
	if err != nil || raw != out.Settings || raw.RepoURL != "" || raw.TLSCert != "" || raw.WorkingHours != "" || raw.CacheGB != 0 {
		t.Fatalf("configuration and automatic defaults diverged: %+v, %v", out.Settings, err)
	}
}

func TestTLSPEMRejectsSpecialFiles(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWebsiteKeyPair(fifo, fifo); err == nil || !strings.Contains(err.Error(), "regular PEM file") {
		t.Fatalf("loading FIFO: %v", err)
	}
}
