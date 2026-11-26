package catalog

import (
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

	"github.com/inkeliz/poudrine/internal/testutil"
)

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
	archive, err := SignedPkg(key, "packagesite.yaml", []byte(strings.Join(localRows, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	put(filepath.Join(local, "packagesite.pkg"), archive)
	root := t.TempDir()
	remoteArchive := filepath.Join(root, "packagesite.pkg")
	put(remoteArchive, testutil.UpstreamFixture(t, PkgSignatureDigest, key, "packagesite.yaml", []byte(upstream), pub, false))
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
	c, err := New(dir, server.URL, trust, local, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := c.Stats(); stats.Packages != 6 || stats.Ahead != 1 || stats.Updated == nil {
		t.Fatalf("unexpected catalog stats %+v", stats)
	}
	snapshot := c.Current.Load()
	original, ok := snapshot.FreeBSDPackage("first")
	if !ok || original.Local || original.Version != "1" || snapshot.Compiled["first"].Version != "2" {
		t.Fatalf("missing source/version comparison: FreeBSD %+v, compiled %+v", original, snapshot.Compiled["first"])
	}
	if len(snapshot.Compiled) != 3 || snapshot.Compiled["second"].Version != "2" || snapshot.compiledAt["second"].IsZero() {
		t.Fatalf("expected the usable local builds, including the older one: %+v", snapshot.Compiled)
	}
	equal, ok := c.Package("All/tie-1.pkg")
	upTie, upstreamOK := snapshot.FreeBSDPackage("tie")
	if !ok || !equal.Local || !upstreamOK || upTie.Sum == equal.Sum || snapshot.offersDownload(upTie) {
		t.Fatalf("equal-version selection or eviction: local %+v, upstream %+v", equal, upTie)
	}
	if _, ok := snapshot.FreeBSDPackage("bonus"); ok || !snapshot.Fetched["bonus"] {
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
		m, err := UnpackCatalog(w.Body, member)
		if err != nil {
			t.Fatal(err)
		}
		sig := m["signature"]
		if len(sig) != c.Key.Size()+1 || rsa.VerifyPKCS1v15(&c.Key.PublicKey, crypto.SHA256, PkgSignatureDigest(m[member]), sig[:len(sig)-1]) != nil {
			t.Fatal("invalid output signature")
		}
		repo, err := ParseCatalog(m[member], member, false)
		if err != nil {
			t.Fatal(err)
		}
		first := repo.ByName["first"]
		if first.Pkg.Version != "2" || !bytes.Contains(first.Raw, []byte("future_field")) {
			t.Fatal(string(first.Raw))
		}
	}

	previous := c.Current.Load()
	if err := c.Refresh(t.Context()); err != nil || unmodified.Load() != 1 || full.Load() != 1 || c.Current.Load() != previous {
		t.Fatalf("unchanged upstream should use 304 and retain the snapshot: %v", err)
	}
	ignoreValidators.Store(true)
	if err := c.Refresh(t.Context()); err != nil || full.Load() != 2 || c.Current.Load() != previous {
		t.Fatalf("identical, reverified 200 response rebuilt the snapshot: %v", err)
	}
	ignoreValidators.Store(false)
	if err := c.Refresh(t.Context()); err != nil || full.Load() != 3 || c.Current.Load() != previous {
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
	previous = c.Current.Load()
	put(remoteArchive, testutil.UpstreamFixture(t, PkgSignatureDigest, key, "packagesite.yaml", []byte(upstream), pub, true))
	for range 2 {
		if err = c.Refresh(t.Context()); err == nil || c.Current.Load() != previous {
			t.Fatal("forged signature accepted")
		}
	}
	put(remoteArchive, testutil.UpstreamFixture(t, PkgSignatureDigest, key, "packagesite.yaml", []byte(upstream), pub, false))
	put(filepath.Join(trust, "revoked", "repo"), fingerprint)
	if err = c.Refresh(context.Background()); err == nil || c.Current.Load() != previous {
		t.Fatal("revoked key accepted")
	}
}

func TestCatalogDataFormat(t *testing.T) {
	catalogs := []string{
		`null`, `[]`, `{"packages":null}`, `{"packages":{}}`,
		`{"packages":false}`, `{"packages":0}`, `{"packages":"[]"}`,
		`{"groups":[]}`, `{"packages":[null]}`, `{"packages":[true]}`,
		`{"packages":[]} {}`, `{"packages":[}`,
		`{"packages":[{"name":"pkg","path":"../private"}]}`,
	}
	for _, b := range catalogs {
		if _, err := ParseCatalog([]byte(b), "data", false); err == nil {
			t.Fatalf("accepted %s", b)
		}
	}
	b := []byte(`{"groups":[{"name":"base"}],"packages":[],"expired_packages":[{"name":"old"}]}`)
	c, err := ParseCatalog(b, "data", false)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(c.extras["groups"]) || !bytes.Contains(c.extras["expired_packages"], []byte("old")) {
		t.Fatal(c)
	}
}

func TestCatalogDataRetainsManifestsAndMetadata(t *testing.T) {
	row := func(version, extra string) string {
		return fmt.Sprintf(`{"name":"pkg","origin":"ports-mgmt/pkg","version":%q,"path":"All/pkg-%s.pkg","pkgsize":3,"sum":"%x","future_field":%q}`, version, version, sha256.Sum256([]byte("pkg")), extra)
	}
	first, newest, tied := row("1", "older"), row("2", "keep me"), row("2", "ignore tie")
	body := []byte(`{"packages":[` + first + ",\n" + newest + "," + tied + `],"groups":null,"future_metadata":{"enabled":true}}`)
	repo, err := ParseCatalog(body, "data", true)
	if err != nil {
		t.Fatal(err)
	}
	got := repo.ByName["pkg"]
	if len(repo.ByName) != 1 || got.Pkg.Version != "2" || !got.Pkg.Local || string(got.Raw) != newest {
		t.Fatalf("lost manifest or changed version selection: %+v", got)
	}
	if string(repo.extras["groups"]) != "null" || string(repo.extras["expired_packages"]) != "[]" || string(repo.extras["future_metadata"]) != `{"enabled":true}` {
		t.Fatalf("changed catalog metadata: %s", repo.extras)
	}
	if _, ok := repo.extras["packages"]; ok {
		t.Fatal("packages leaked into metadata")
	}
	clear(body)
	if string(got.Raw) != newest {
		t.Fatal("retained manifest aliases the input buffer")
	}
}

func TestPackageChecksums(t *testing.T) {
	for _, sum := range []string{
		"2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
		"1$2C26B46B68FFC68FF99B453C1D30413413422D706483BFA0F98A5E886266E7AE",
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

func TestPackageChecksumValidation(t *testing.T) {
	for _, sum := range []string{"", "1$", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), strings.Repeat("é", 32), strings.Repeat("a", 63) + " ", "99$" + strings.Repeat("a", 64)} {
		if _, err := packageHasher(sum); err == nil {
			t.Errorf("accepted invalid checksum %q", sum)
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
		if got := ComparePkgVersions(p.a, p.b); got != p.want {
			t.Errorf("%s vs %s = %d, want %d", p.a, p.b, got, p.want)
		}
	}
}

func TestCatalogPublishesExistingSigningKey(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	open := func() *Catalog {
		t.Helper()
		c, err := New(state, "https://repo.example", "", "", nil)
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
