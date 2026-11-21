package poudrine

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestOfflineClientSetup(t *testing.T) {
	a := testAdmin(t)
	a.cfg.RepositoryURL = "http://pkg.example:8080/lan"
	data, err := makeClientInstructions(a.cfg.RepositoryURL, testRepositoryPublicKey)
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
	if err := os.WriteFile(filepath.Join(state, "repository.pub"), []byte(testRepositoryPublicKey), 0644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	o := options{state: state, publicURL: a.cfg.RepositoryURL}
	if err := printClientSetup(o, &output); err != nil || output.String() != data.SetupCommand {
		t.Fatalf("offline setup and website differ: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "pkg")
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(data.SetupCommand, "/usr/local/etc/pkg", dir))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copied setup: %v: %s", err, out)
	}
	for path, want := range map[string]string{
		"poudrine.pub":           testRepositoryPublicKey,
		"repos/zz-Poudrine.conf": strings.ReplaceAll(data.RepoConfig, "/usr/local/etc/pkg", dir),
	} {
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || string(got) != want {
			t.Fatalf("incorrect client file %s: %v", path, err)
		}
	}
	o.publicURL, o.repoListen = "", "[fd00::42]:9080"
	if addr, err := o.advertisedRepository(0); err != nil || addr != "http://[fd00::42]:9080" {
		t.Fatalf("direct IPv6 repository: %q, %v", addr, err)
	}
	o.repoListen = "[::1]:0"
	if _, err := o.advertisedRepository(0); err == nil {
		t.Fatal("advertised an unknown ephemeral port offline")
	}
}

func TestCopiedReportCommand(t *testing.T) {
	for _, binary := range []string{"curl", "openssl"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skip("requires " + binary)
		}
	}
	const prefix = "/network/o'neil"
	repo, _ := inventoryReportFixture(t)
	received := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != prefix+reportPath || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		received <- string(body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.URL.Path, r.URL.RawPath = reportPath, ""
		repo.ServeHTTP(w, r)
	}))
	defer server.Close()
	data, err := makeClientInstructions(server.URL+prefix, testRepositoryPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	bin, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "pkg"), []byte("#!/bin/sh\nprintf '%s' \"$TEST_ROWS\"\nexit \"$TEST_PKG_STATUS\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	var public []byte
	for _, tc := range []struct{ name, rows, status string }{
		{"installed", "pkg 1.2_3,1 ports-mgmt/pkg\ncurl 8.0 ftp/curl\n", "0"},
		{"empty", "", "0"},
		{"failed-query", "partial", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh")
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_ROWS="+tc.rows, "TEST_PKG_STATUS="+tc.status)
			cmd.Stdin = strings.NewReader(strings.ReplaceAll(data.ReportCommand, "/var/db/poudrine-client", state))
			out, err := cmd.CombinedOutput()
			if tc.status != "0" {
				if err == nil || len(received) != 0 {
					t.Fatal("failed pkg query submitted a report")
				}
				return
			}
			if err != nil {
				t.Fatalf("copied signed report: %v: %s", err, out)
			}
			if got := <-received; got != tc.rows {
				t.Fatalf("report body %q, want %q", got, tc.rows)
			}
			var pin []byte
			if err := repo.store.db.QueryRow("SELECT public_key FROM client_report_keys").Scan(&pin); err != nil {
				t.Fatal(err)
			}
			if public != nil && !bytes.Equal(pin, public) {
				t.Fatal("second report did not reuse the client key")
			}
			public = pin
			if st, err := os.Stat(filepath.Join(state, "inventory.key")); err != nil || st.Mode().Perm() != 0600 {
				t.Fatal("expected private reporting key with mode 0600", err)
			}
		})
	}
}

// Exercise pkg's actual config merge and archive signature validation while
// keeping all client configuration and the pkg database inside a temp tree.
func TestSetupFreeBSDClient(t *testing.T) {
	if runtime.GOOS != "freebsd" {
		t.Skip("requires FreeBSD pkg")
	}
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
	content, err := signedPkg(key, "data", []byte(`{"packages":[],"groups":[],"expired_packages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(serverFiles, "meta.conf"), []byte(repoMeta), 0600)
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
	forged, err := signedPkg(foreign, "data", []byte(`{"packages":[],"groups":[],"expired_packages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(serverFiles, "data.pkg"), forged, 0600)
	run(false)
}
