package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestConfigDocumentValidation(t *testing.T) {
	valid := []string{
		`{}`, `{"web_tls":false,"cache_gb":0.0e100}`, `{"cache_gb":0.000000001}`,
		`{"state":"./data-\ud83c\udf35"}`, `{"state":"./data-\ufffd\\ud800"}`,
		`{"make_jobs_number":8,"make_jobs_number_limit":4}`,
	}
	for _, raw := range valid {
		if _, err := DecodeConfiguration([]byte(raw), ""); err != nil {
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
		_, err := DecodeConfiguration([]byte(tc.raw), "")
		if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), tc.problem) {
			t.Errorf("validating %q: want %q, got %v", tc.raw, tc.problem, err)
		}
	}
}

func TestTLSPEMRejectsSpecialFiles(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWebsiteKeyPair(fifo, fifo); err == nil || !strings.Contains(err.Error(), "regular PEM file") {
		t.Fatalf("loading FIFO: %v", err)
	}
}
