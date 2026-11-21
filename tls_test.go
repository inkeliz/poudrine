package poudrine

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedWebTLSCertificate(t *testing.T) {
	state := t.TempDir()
	load := func(host string) *tls.Config {
		t.Helper()
		conf, err := webTLSConfig(state, "[::]:8181", "https://"+host+"/admin", "", "")
		if err != nil {
			t.Fatal(err)
		}
		return conf
	}
	conf := load("packages.example.org")
	if conf.MinVersion < tls.VersionTLS12 {
		t.Fatal("weak TLS minimum")
	}
	crt := filepath.Join(state, "web-tls.crt")
	key := filepath.Join(state, "web-tls.key")
	if s, err := os.Stat(key); err != nil || s.Mode().Perm() != 0600 {
		t.Fatal("key must be private", err)
	}
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	certPEM := read(crt)
	keyPEM := read(key)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("invalid public certificate")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.TLS = conf
	server.StartTLS()
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "packages.example.org"}}}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	cert := response.TLS.PeerCertificates[0]
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"::1", "127.0.0.1", "localhost", host, "packages.example.org"} {
		if err := cert.VerifyHostname(name); err != nil {
			t.Fatal(err)
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if address, ok := a.(*net.IPNet); ok && !address.IP.IsMulticast() && !address.IP.IsUnspecified() {
				if err := cert.VerifyHostname(address.IP.String()); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	load("packages.example.org")
	if !bytes.Equal(read(crt), certPEM) || !bytes.Equal(read(key), keyPEM) {
		t.Fatal("TLS identity changed across restart")
	}
	load("new.example.org")
	if bytes.Equal(read(crt), certPEM) || !bytes.Equal(read(key), keyPEM) {
		t.Fatal("changed DNS name should reissue the certificate with the original key")
	}
	block, _ := pem.Decode(read(crt))
	reissued, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := reissued.VerifyHostname("new.example.org"); err != nil {
		t.Fatal(err)
	}
	// An absent public cert is recoverable using the retained key.
	if err := os.Remove(crt); err != nil {
		t.Fatal(err)
	}
	load("new.example.org")
	if !bytes.Equal(read(key), keyPEM) {
		t.Fatal("key changed while restoring public certificate")
	}
}

func TestExternalWebTLSCertificate(t *testing.T) {
	original, external := t.TempDir(), t.TempDir()
	bind := "[2001:db8::8%vnet0]:8181"
	c, err := webTLSConfig(original, bind, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cert, key := filepath.Join(original, "web-tls.crt"), filepath.Join(original, "web-tls.key")
	conf, err := webTLSConfig(external, bind, "", cert, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.Certificates[0].Certificate[0], conf.Certificates[0].Certificate[0]) {
		t.Fatal("custom certificate not used")
	}
	parsed, err := x509.ParseCertificate(conf.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.VerifyHostname("2001:db8::8"); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(external)
	if err != nil || len(files) != 0 {
		t.Fatal("unexpected self-signed files", err)
	}
	if _, err := webTLSConfig(external, bind, "", cert, ""); err == nil {
		t.Fatal("accepted incomplete custom pair")
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if _, err := webTLSConfig(original, bind, "", "", ""); err == nil {
		t.Fatal("silently replaced missing private key")
	}
}
