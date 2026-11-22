package admin

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/catalog"
	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/public"
	"github.com/inkeliz/poudrine/internal/testutil"
)

func TestPageLocale(t *testing.T) {
	a := testAdmin(t)
	updated := time.Date(2026, 11, 22, 12, 34, 56, 0, time.UTC)
	a.cfg.Stats = func() catalog.CatalogStats {
		return catalog.CatalogStats{Packages: 1234567, Ahead: 1234, Updated: &updated}
	}
	for _, path := range []string{"/", "/admin"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Accept-Language", "en;q=0.1, hi-IN")
		w := adminResponse(t, a, r, http.StatusOK)
		if w.Header().Get("Content-Language") != "hi-IN" || w.Header().Get("Vary") != "Accept-Language" || !strings.Contains(w.Body.String(), `data-locale="hi-IN"`) {
			t.Fatalf("%s did not expose the requested format locale", path)
		}
		if path == "/" {
			for _, want := range []string{">12,34,567</strong>", ">1,234</strong>", `datetime="2026-11-22T12:34:56Z"`} {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("%s missing %q", path, want)
				}
			}
		}
	}
}

func TestOfflineClientSetup(t *testing.T) {
	a := testAdmin(t)
	a.cfg.RepositoryURL = "http://pkg.example:8080/lan"
	data, err := public.MakeClientInstructions(a.cfg.RepositoryURL, testutil.RepositoryPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	page := adminResponse(t, a, httptest.NewRequest("GET", "/", nil), 200)
	rendered := html.UnescapeString(page.Body.String())
	for _, source := range []string{data.SetupCommand, data.ReportCommand, data.Fingerprint, data.PublicKey, data.RepoConfig} {
		if !strings.Contains(rendered, source) {
			t.Fatal("setup page is missing a server-rendered client value")
		}
	}

	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "repository.pub"), []byte(testutil.RepositoryPublicKey), 0644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	o := config.Options{State: state, PublicURL: a.cfg.RepositoryURL}
	if err := public.PrintClientSetup(o, &output); err != nil || output.String() != data.SetupCommand {
		t.Fatalf("offline setup and website differ: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "pkg")
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(data.SetupCommand, "/usr/local/etc/pkg", dir))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copied setup: %v: %s", err, out)
	}
	for path, want := range map[string]string{
		"poudrine.pub":           testutil.RepositoryPublicKey,
		"repos/zz-Poudrine.conf": strings.ReplaceAll(data.RepoConfig, "/usr/local/etc/pkg", dir),
	} {
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || string(got) != want {
			t.Fatalf("incorrect client file %s: %v", path, err)
		}
	}
	o.PublicURL, o.RepoListen = "", "[fd00::42]:9080"
	if addr, err := o.AdvertisedRepository(0); err != nil || addr != "http://[fd00::42]:9080" {
		t.Fatalf("direct IPv6 repository: %q, %v", addr, err)
	}
	o.RepoListen = "[::1]:0"
	if _, err := o.AdvertisedRepository(0); err == nil {
		t.Fatal("advertised an unknown ephemeral port offline")
	}
}

// Exercise pkg's actual config merge and archive signature validation while
// keeping all client configuration and the pkg database inside a temp tree.
func TestSetupFreeBSDClient(t *testing.T) {
	if _, err := os.Stat("/usr/local/sbin/pkg"); err != nil {
		t.Skip("pkg is not installed")
	}
	dir := t.TempDir()
	serverFiles := filepath.Join(dir, "upstream")
	for _, sub := range []string{serverFiles, filepath.Join(dir, "db"), filepath.Join(dir, "base-repos")} {
		if err := os.Mkdir(sub, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, data, mode); err != nil {
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
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	content, err := catalog.SignedPkg(key, "data", []byte(`{"packages":[],"groups":[],"expired_packages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(serverFiles, "meta.conf"), []byte(catalog.RepoMeta), 0600)
	write(filepath.Join(serverFiles, "data.pkg"), content, 0600)
	server := httptest.NewServer(http.FileServer(http.Dir(serverFiles)))
	defer server.Close()

	a := testAdmin(t)
	a.cfg.RepositoryURL = server.URL
	a.cfg.PublicKey = func() string { return pub }
	download := adminResponse(t, a, httptest.NewRequest(http.MethodGet, "/setup-client.sh", nil), 200)
	if download.Header().Get("Content-Disposition") != `attachment; filename="setup-client.sh"` {
		t.Fatal("missing script filename")
	}

	etc := filepath.Join(dir, "client-etc")
	script := strings.ReplaceAll(download.Body.String(), "/usr/local/etc/pkg", etc)
	if script == download.Body.String() {
		t.Fatal("could not isolate installation directory")
	}
	write(filepath.Join(dir, "setup.sh"), []byte(script), 0600)
	write(filepath.Join(dir, "pkg"), []byte("#!/bin/sh\nexec /usr/local/sbin/pkg -o INSTALL_AS_USER=yes -C \"$TEST_PKG_CONF\" \"$@\"\n"), 0700)

	repos := filepath.Join(etc, "repos")
	pkgconf := filepath.Join(dir, "pkg.conf")
	write(pkgconf, []byte(fmt.Sprintf(`REPOS_DIR: [%q,%q]
INSTALL_AS_USER: yes
PKG_DBDIR: %q
`, filepath.Join(dir, "base-repos"), repos, filepath.Join(dir, "db"))), 0600)
	var upstreamConf strings.Builder
	for _, name := range []string{"FreeBSD", "FreeBSD-ports", "FreeBSD-ports-kmods", "FreeBSD-base", "Other"} {
		fmt.Fprintf(&upstreamConf, "%s: { url: \"https://example.org\", enabled: yes }\n", name)
	}
	write(filepath.Join(dir, "base-repos", "FreeBSD.conf"), []byte(upstreamConf.String()), 0600)
	run := func(succeed bool) {
		t.Helper()
		cmd := exec.Command("sh", filepath.Join(dir, "setup.sh"))
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TEST_PKG_CONF="+pkgconf, "PATH="+dir+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if (err == nil) != succeed {
			t.Fatalf("setup: %v\n%s", err, out)
		}
	}
	repoFile := filepath.Join(repos, "zz-Poudrine.conf")
	keyFile := filepath.Join(etc, "poudrine.pub")
	run(true)
	// Rerunning on an older Poudrine installation removes its FreeBSD override.
	write(repoFile, []byte("FreeBSD: { enabled: no }\n"), 0644)
	run(true)
	if got, err := os.ReadFile(keyFile); err != nil || string(got) != pub {
		t.Fatal("wrong installed key", err)
	}

	out, err := exec.Command("/usr/local/sbin/pkg", "-o", "INSTALL_AS_USER=yes", "-C", pkgconf, "-vv").CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	for name, enabled := range map[string]string{"Poudrine": "yes", "FreeBSD": "yes", "FreeBSD-ports": "no", "FreeBSD-ports-kmods": "yes", "FreeBSD-base": "yes", "Other": "yes"} {
		if !regexp.MustCompile(name + `:\s*\{[^}]*enabled\s*:\s*` + enabled + `\b`).Match(out) {
			t.Fatalf("unexpected repo %s configuration:\n%s", name, out)
		}
	}
	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := catalog.SignedPkg(foreign, "data", []byte(`{"packages":[],"groups":[],"expired_packages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(serverFiles, "data.pkg"), forged, 0600)
	run(false)
}
