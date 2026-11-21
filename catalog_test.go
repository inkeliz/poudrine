package poudrine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Build the detached-signature archive layout used by pkg.FreeBSD.org.
func upstreamFixture(t *testing.T, key *rsa.PrivateKey, member string, body, pub []byte, forged bool) []byte {
	t.Helper()
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, pkgSignatureDigest(body))
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

func TestCatalogTrustMergeAndRefresh(t *testing.T) {
	dir := t.TempDir()
	trust := filepath.Join(dir, "keys")
	local := filepath.Join(dir, "local")
	for _, d := range []string{filepath.Join(trust, "trusted"), filepath.Join(trust, "revoked"), filepath.Join(local, "All")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	put := func(p string, b []byte) {
		t.Helper()
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	fingerprint := []byte(fmt.Sprintf("function: \"sha256\"\nfingerprint: \"%x\"\n", sha256.Sum256(pub)))
	trustedFile := filepath.Join(trust, "trusted", "repo")
	put(trustedFile, fingerprint)
	row := func(name, version, abi string) string {
		return fmt.Sprintf(`{"name":%q,"origin":"test/fixture","version":%q,"abi":%q,"path":"All/%s-%s.pkg","pkgsize":3,"sum":"%x","annotations":{"flavor":"one","subpackage":"lib"},"future_field":true}`, name, version, abi, name, version, sha256.Sum256([]byte("pkg")))
	}
	const native = "FreeBSD:15:amd64"
	upstream := strings.Join([]string{row("first", "1", native), row("second", "1,2", native), row("third", "1", native), row("tie", "1", native), row("broken", "1", native)}, "\n")
	hashedBonus := strings.Replace(row("bonus", "1", "FreeBSD:15:*"), "All/bonus-1.pkg", "All/Hashed/bonus-1~1234.pkg", 1)
	tied := strings.Replace(row("tie", "1", native), fmt.Sprintf("%x", sha256.Sum256([]byte("pkg"))), fmt.Sprintf("%x", sha256.Sum256([]byte("tie"))), 1)
	localRows := []string{row("first", "2", native), row("second", "2", native), row("third", "2", native), hashedBonus, row("other", "1", "FreeBSD:15:aarch64"), tied, row("broken", "1", native)}
	// Missing third-2 leaves the upstream third-1 selected.
	for _, name := range []string{"first-2", "second-2", "other-1"} {
		put(filepath.Join(local, "All", name+".pkg"), []byte("pkg"))
	}
	put(filepath.Join(local, "All", "tie-1.pkg"), []byte("tie"))
	put(filepath.Join(local, "All", "broken-1.pkg"), []byte("bad"))
	if err := os.Mkdir(filepath.Join(local, ".pkg-cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(local, "All", "Hashed"), 0700); err != nil {
		t.Fatal(err)
	}
	put(filepath.Join(local, ".pkg-cache", "bonus-1.pkg"), []byte("pkg"))
	if err := os.Link(filepath.Join(local, ".pkg-cache", "bonus-1.pkg"), filepath.Join(local, "All", "Hashed", "bonus-1~1234.pkg")); err != nil {
		t.Fatal(err)
	}
	archive, err := signedPkg(key, "packagesite.yaml", []byte(strings.Join(localRows, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	put(filepath.Join(local, "packagesite.pkg"), archive)
	root := t.TempDir()
	remoteArchive := filepath.Join(root, "packagesite.pkg")
	put(remoteArchive, upstreamFixture(t, key, "packagesite.yaml", []byte(upstream), pub, false))
	var ignoreValidators, changeMirror atomic.Bool
	var full, unmodified atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/packagesite.pkg" && changeMirror.Load() {
			http.Redirect(w, r, "/mirror/packagesite.pkg", http.StatusFound)
			return
		}
		if r.URL.Path != "/packagesite.pkg" && r.URL.Path != "/mirror/packagesite.pkg" {
			if r.Header.Get("If-None-Match") != "" {
				t.Error("sent a validator for the wrong archive")
			}
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(remoteArchive)
		if err != nil {
			http.Error(w, "missing fixture", 500)
			return
		}
		etag := fmt.Sprintf("\"%x\"", sha256.Sum256(b))
		if !ignoreValidators.Load() {
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				unmodified.Add(1)
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		full.Add(1)
		w.Write(b)
	}))
	defer server.Close()
	c, err := NewCatalog(dir, server.URL, trust, local, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := c.Stats(); stats.Packages != 6 || stats.Ahead != 1 || stats.Updated == nil {
		t.Fatalf("unexpected catalog stats %+v", stats)
	}
	snapshot := c.current.Load()
	original, ok := snapshot.freeBSDPackage("first")
	if !ok || original.Local || original.Version != "1" || snapshot.compiled["first"].Version != "2" {
		t.Fatalf("missing source/version comparison: FreeBSD %+v, compiled %+v", original, snapshot.compiled["first"])
	}
	if len(snapshot.compiled) != 3 || snapshot.compiled["second"].Version != "2" || snapshot.compiledAt["second"].IsZero() {
		t.Fatalf("expected the usable local builds, including the older one: %+v", snapshot.compiled)
	}
	equal, ok := c.Package("All/tie-1.pkg")
	upTie, upstreamOK := snapshot.freeBSDPackage("tie")
	if !ok || !equal.Local || !upstreamOK || upTie.Sum == equal.Sum || !snapshot.supersedes(upTie) {
		t.Fatalf("equal-version selection or eviction: local %+v, upstream %+v", equal, upTie)
	}
	if _, ok := snapshot.freeBSDPackage("bonus"); ok || !snapshot.fetched["bonus"] {
		t.Fatal("local-only dependency fetched by Poudriere misclassified")
	}
	for p, local := range map[string]bool{"first-2": true, "second-1,2": false, "third-1": false, "Hashed/bonus-1~1234": true, "broken-1": false} {
		got, ok := c.Package("All/" + p + ".pkg")
		if !ok || got.Local != local || got.BuildOrigin != "test/fixture@one~lib" {
			t.Errorf("%s: %+v", p, got)
		}
	}
	for alias, member := range map[string]string{"data.pkg": "data", "packagesite.txz": "packagesite.yaml"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/"+alias, nil)
		if !c.Metadata(w, r, alias) || w.Code != 200 {
			t.Fatal(w.Code)
		}
		m, err := unpackCatalog(w.Body, member)
		if err != nil {
			t.Fatal(err)
		}
		sig := m["signature"]
		if len(sig) != c.key.Size()+1 || rsa.VerifyPKCS1v15(&c.key.PublicKey, crypto.SHA256, pkgSignatureDigest(m[member]), sig[:len(sig)-1]) != nil {
			t.Fatal("invalid output signature")
		}
		repo, err := parseCatalog(m[member], member, false)
		if err != nil {
			t.Fatal(err)
		}
		first := repo.byName["first"]
		if first.pkg.Version != "2" || !bytes.Contains(first.raw, []byte("future_field")) {
			t.Fatal(string(first.raw))
		}
	}

	previous := c.current.Load()
	if err := c.Refresh(t.Context()); err != nil || unmodified.Load() != 1 || full.Load() != 1 || c.current.Load() != previous {
		t.Fatalf("unchanged upstream should use 304 and retain the snapshot: %v", err)
	}
	ignoreValidators.Store(true)
	if err := c.Refresh(t.Context()); err != nil || full.Load() != 2 || c.current.Load() != previous {
		t.Fatalf("identical, reverified 200 response rebuilt the snapshot: %v", err)
	}
	ignoreValidators.Store(false)
	if err := c.Refresh(t.Context()); err != nil || full.Load() != 3 || c.current.Load() != previous {
		t.Fatalf("could not resume ETag validation: %v", err)
	}
	// The local index is unchanged while a previously missing archive appears.
	put(filepath.Join(local, "All", "third-2.pkg"), []byte("pkg"))
	if err := c.Refresh(t.Context()); err != nil || full.Load() != 4 || c.Stats().Ahead != 2 {
		t.Fatalf("missed newly built local package: %+v, %v", c.Stats(), err)
	}
	if err := os.Remove(filepath.Join(local, "All", "first-2.pkg")); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(t.Context()); err != nil || full.Load() != 5 || c.Stats().Ahead != 1 {
		t.Fatalf("missed removed local package: %+v, %v", c.Stats(), err)
	}
	changeMirror.Store(true)
	if err := c.Refresh(t.Context()); err != nil || full.Load() != 6 || unmodified.Load() != 2 {
		t.Fatalf("failed to revalidate a redirect to a different mirror: %v", err)
	}

	// A rejected replacement can never displace the published snapshot
	// or save an ETag against which a retry could incorrectly receive 304.
	previous = c.current.Load()
	put(remoteArchive, upstreamFixture(t, key, "packagesite.yaml", []byte(upstream), pub, true))
	for range 2 {
		if err = c.Refresh(t.Context()); err == nil || c.current.Load() != previous {
			t.Fatal("forged signature accepted")
		}
	}
	put(remoteArchive, upstreamFixture(t, key, "packagesite.yaml", []byte(upstream), pub, false))
	put(filepath.Join(trust, "revoked", "repo"), fingerprint)
	if err = c.Refresh(context.Background()); err == nil || c.current.Load() != previous {
		t.Fatal("revoked key accepted")
	}
}

func TestDefaultCatalogPoll(t *testing.T) {
	o, err := defaultConfiguration().options(defaultConfigPath)
	if err != nil || o.refresh != 5*time.Minute {
		t.Fatalf("default catalog polling interval %s: %v", o.refresh, err)
	}
}

func TestCatalogDataFormat(t *testing.T) {
	catalogs := []string{
		`{"packages":false}`,
		`{"groups":[]}`,
		`{"packages":[{"name":"pkg","path":"../private"}]}`,
	}
	for _, b := range catalogs {
		if _, err := parseCatalog([]byte(b), "data", false); err == nil {
			t.Fatalf("accepted %s", b)
		}
	}
	b := []byte(`{"groups":[{"name":"base"}],"packages":[],"expired_packages":[{"name":"old"}]}`)
	c, err := parseCatalog(b, "data", false)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(c.extras["groups"]) || !bytes.Contains(c.extras["expired_packages"], []byte("old")) {
		t.Fatal(c)
	}
}

func TestPackageChecksums(t *testing.T) {
	for _, sum := range []string{
		"2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
		"2$kgygnaah7wxsgn1wkuic4j78zq8dicmx53picmma99ogmkbd7k5nhuxr5xxemz6yzjab15oor3tjt7nupj8mh764y7kddbne7qw9agn",
		"5$eoiiccdoiuz9acwfo7fxi6abnrfdtg81mz5ccx7tbg5ny9755g7y",
	} {
		h, err := packageHasher(sum)
		if err != nil {
			t.Fatal(err)
		}
		h.Write([]byte("foo"))
		if !packageHashMatches(sum, h) {
			t.Fatal(sum)
		}
		h.Write([]byte("bar"))
		if packageHashMatches(sum, h) {
			t.Fatal("bad digest accepted")
		}
	}
}

func TestPkgVersions(t *testing.T) {
	for _, p := range []struct {
		a, b string
		want int
	}{
		{"1.0", "1", 0}, {"3", "2,1", -1}, {"3_1,1", "3_2,1", -1}, {"2.1", "2alpha", 1}, {"2.pl1", "2.alpha1", -1}, {"1rc2", "1", -1}, {"1.2+1", "1.2", 1}, {"foo-10beta", "foo-10.a", 1}, {"2.*", "2pl1", -1}, {"1.2", "1.10", -1},
	} {
		if got := comparePkgVersions(p.a, p.b); got != p.want {
			t.Errorf("%s vs %s = %d, want %d", p.a, p.b, got, p.want)
		}
	}
}

func TestCatalogPublishesExistingSigningKey(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	open := func() *Catalog {
		t.Helper()
		c, err := NewCatalog(state, "https://repo.example", "", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	initial := open()
	publicPath := filepath.Join(state, "repository.pub")
	data, err := os.ReadFile(publicPath)
	if err != nil || !bytes.Equal(data, initial.PublicKey) {
		t.Fatalf("published public key differs from the catalog's key: %v", err)
	}
	if err := os.Remove(publicPath); err != nil {
		t.Fatal(err)
	}
	restarted := open()
	data, err = os.ReadFile(publicPath)
	if err != nil || !bytes.Equal(data, initial.PublicKey) || !bytes.Equal(data, restarted.PublicKey) {
		t.Fatalf("restarting with an existing signing key changed the published key: %v", err)
	}
	key, err := os.Stat(filepath.Join(state, "repository.key"))
	if err != nil {
		t.Fatal(err)
	}
	if key.Mode().Perm()&0077 != 0 {
		t.Fatalf("private key permissions %v", key.Mode())
	}
}
