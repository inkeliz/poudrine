package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
