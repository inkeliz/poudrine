// Package testutil contains shared fixtures for the application tests.
package testutil

import (
	"archive/tar"
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/packageinfo"
	"github.com/inkeliz/poudrine/internal/requestauth"
)

const RepositoryPublicKey = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAxO3nFracWdZGsp07HTdd
7pH8+4qRKy5AMsf3618fzgW4uEH3HB5+6J2+kIE2Wgt4J/0/p+nGcgtuexI7m2w9
P+I/bDgRmb9dkzWTJEQn96jyHm4JMzIHLvg5kHFb6+hAzkdlzBIuMjkm9ojpV7aA
8J3+Vs8NfCM1ueKcq9CKQs4WF9W+Np5BgRmMNkJkMGYmDYeRktVLZe+zBh5FsuNM
RqbRh9vJj+EF6T5DSHyvPjW+j4tfWBIbuDKuXh28RLWzUfslvOUeLEQLU4mqb1qw
f7Zw6RSrb4mLc8XENzjLz8KHnolRqPbxRGvNUmZNEzr7mn8UraVr7nHvctHP2dn1
eQIDAQAB
-----END PUBLIC KEY-----
`

func AdminKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func SignedRequest(t *testing.T, key ed25519.PrivateKey, method, path string, body []byte, ts time.Time) *http.Request {
	t.Helper()
	var raw [16]byte
	rand.Read(raw[:])
	nonce := base64.RawURLEncoding.EncodeToString(raw[:])
	stamp := fmt.Sprint(ts.Unix())
	prefix := fmt.Sprintf("poudrine-v2\n%s\n%s\n%s\n%s\n", method, path, stamp, nonce)
	msg := append([]byte(prefix), body...)
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("X-Poudrine-Key", base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
	r.Header.Set("X-Poudrine-Time", stamp)
	r.Header.Set("X-Poudrine-Nonce", nonce)
	r.Header.Set("X-Poudrine-Signature", base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, msg)))
	return r
}

func WorkerConfig(root string) config.Builder {
	cfg := config.DefaultBuilder()
	cfg.ConfigDir = filepath.Join(root, "config")
	cfg.BaseFS = filepath.Join(root, "base")
	cfg.Distfiles = filepath.Join(root, "distfiles")
	cfg.SigningKey = filepath.Join(root, "signing.key")
	cfg.PackagesFile = filepath.Join(root, "origins")
	cfg.StatusFile = filepath.Join(root, "status.json")
	cfg.Release = "15.1-RELEASE"
	return cfg
}

func WriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func ReportKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
}

func Manifest(name, version string) string {
	return OriginManifest(name, "devel/"+name, version)
}

func OriginManifest(name, origin, version string) string {
	return fmt.Sprintf(`{"name":%q,"origin":%q,"version":%q,"abi":"FreeBSD:15:amd64","arch":"freebsd:15:x86:64","comment":"Test package","desc":"Test package","maintainer":"test@example.org","www":"https://example.org","prefix":"/usr/local","flatsize":0,"licenselogic":"single"}`, name, origin, version)
}

func Archive(t *testing.T, manifest string) []byte {
	t.Helper()
	var output bytes.Buffer
	tw := tar.NewWriter(&output)
	for _, member := range []string{"+COMPACT_MANIFEST", "+MANIFEST"} {
		if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0644, Size: int64(len(manifest))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func Package(name, body string) packageinfo.Package {
	return packageinfo.Package{Name: name, Version: "1", Path: "All/" + name + ".pkg", Origin: "devel/" + name, Sum: fmt.Sprintf("%x", sha256.Sum256([]byte(body))), Size: int64(len(body))}
}

func Time(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// ReportProof uses a consistent signer and a fresh nonce for each client report.
func ReportProof() requestauth.ReportProof {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	return requestauth.ReportProof{Key: ReportKey().Public().(ed25519.PublicKey), Nonce: nonce, Stamp: time.Now().Unix()}
}

// Build the detached-signature archive layout used by pkg.FreeBSD.org.
func UpstreamFixture(t *testing.T, digest func([]byte) []byte, key *rsa.PrivateKey, member string, body, pub []byte, forged bool) []byte {
	t.Helper()
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest(body))
	if err != nil {
		t.Fatal(err)
	}
	if forged {
		sig[0] ^= 1
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range []struct {
		name string
		b    []byte
	}{{member + ".sig", sig}, {member + ".pub", pub}, {member, body}} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Size: int64(len(e.b)), Mode: 0644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.b); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
