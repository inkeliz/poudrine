package config

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSetTLSConfiguration(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	config := filepath.Join(dir, "poudrine.json")
	original := []byte(`{"web_tls":false,"jobs":4,"repo_url":"https://repo.example"}`)
	if err := os.WriteFile(config, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := SetTLS([]string{"-config", config, "-tls-cert", certPath}); err == nil {
		t.Fatal("accepted incomplete certificate")
	}
	if err := SetTLS([]string{"-config", config, "-tls-cert", certPath, "-tls-key", keyPath}); err == nil {
		t.Fatal("accepted missing certificate")
	}
	if b, err := os.ReadFile(config); err != nil || string(b) != string(original) {
		t.Fatal("failed validation changed config", err)
	}
	s := httptest.NewTLSServer(http.NotFoundHandler())
	defer s.Close()
	pair := s.TLS.Certificates[0]
	der, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{certPath: {Type: "CERTIFICATE", Bytes: pair.Certificate[0]}, keyPath: {Type: "PRIVATE KEY", Bytes: der}} {
		if err := os.WriteFile(name, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := SetTLS([]string{"-config", config, "-tls-cert", certPath, "-tls-key", keyPath}); err != nil {
		t.Fatal(err)
	}
	c, _, err := LoadConfiguration(config)
	if err != nil || !c.WebTLS || c.TLSCert != certPath || c.TLSKey != keyPath || c.Jobs != 4 || c.RepoURL != "https://repo.example" {
		t.Fatalf("unexpected custom settings: %+v, %v", c, err)
	}
	if err := SetTLS([]string{"-config", config, "-self-signed"}); err != nil {
		t.Fatal(err)
	}
	c, _, err = LoadConfiguration(config)
	if err != nil || !c.WebTLS || c.TLSCert != "" || c.TLSKey != "" || c.Jobs != 4 {
		t.Fatalf("unexpected self-signed settings: %+v, %v", c, err)
	}
}

func TestTLSConfigDefaults(t *testing.T) {
	for _, pair := range []struct {
		raw   string
		valid bool
	}{
		{`{}`, true},
		{`{"tls_cert":"cert.pem"}`, false},
		{`{"tls_key":"key.pem"}`, false},
		{`{"tls_cert":"cert.pem","tls_key":"key.pem"}`, true},
	} {
		c, err := DecodeConfiguration([]byte(pair.raw), "")
		if pair.valid != (err == nil) {
			t.Fatalf("decoding %s: %v", pair.raw, err)
		}
		if pair.valid {
			o, err := c.Options("/srv/poudrine/service.json")
			if err != nil || !o.WebTLS || c.TLSCert != "" && (o.TLSCert != "/srv/poudrine/cert.pem" || o.TLSKey != "/srv/poudrine/key.pem") {
				t.Fatalf("unexpected TLS options for %s", pair.raw)
			}
		}
	}
}
