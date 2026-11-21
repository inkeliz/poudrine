package poudrine

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	texttemplate "text/template"

	"github.com/inkeliz/poudrine/web/poudrine"
)

var setupClientTemplate = texttemplate.Must(texttemplate.ParseFS(poudrineweb.Files, "setup-client.sh.tmpl"))
var reportClientTemplate = texttemplate.Must(texttemplate.ParseFS(poudrineweb.Files, "report-inventory.sh.tmpl"))
var repositoryPage = template.Must(template.ParseFS(poudrineweb.Files, "index.html"))

type clientInstructions struct {
	RepositoryURL, RepoConfig, PublicKey, Fingerprint string
	SetupCommand, ReportCommand                       string
}

// The website can resolve an ephemeral listener. On the host console, use
// the configured port or the full address supplied by the operator.
func (o options) advertisedRepository(boundPort int) (string, error) {
	if o.publicURL != "" {
		return o.publicURL, nil
	}
	host, port, err := net.SplitHostPort(o.repoListen)
	if err != nil {
		return "", err
	}
	if boundPort != 0 {
		port = strconv.Itoa(boundPort)
	} else if port == "0" {
		return "", errors.New("set repo_url or a fixed repo_listen port to generate client setup offline")
	}
	if host == "" || net.ParseIP(host).IsUnspecified() {
		host, err = os.Hostname()
		if err != nil {
			return "", err
		}
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}).String(), nil
}

func makeClientInstructions(address, publicKey string) (clientInstructions, error) {
	var data clientInstructions
	u, err := webURL(address)
	if err != nil {
		return data, err
	}
	// Only the canonical public PEM goes into the quoted shell here-document.
	block, rest := pem.Decode([]byte(publicKey))
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return data, errors.New("invalid repository public key")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if _, ok := key.(*rsa.PublicKey); err != nil || !ok {
		return data, errors.New("expected an RSA repository public key")
	}
	data.RepositoryURL = u.String()
	data.PublicKey = string(pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: block.Bytes}))
	data.Fingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(data.PublicKey)))
	data.RepoConfig = fmt.Sprintf(`Poudrine: {
  url: %s,
  pubkey: "/usr/local/etc/pkg/poudrine.pub",
  signature_type: "pubkey",
  enabled: yes
}
FreeBSD-ports: { enabled: no }
`, strconv.Quote(data.RepositoryURL))
	var setup strings.Builder
	setup.WriteString("sh <<'POUDRINE_SETUP'\n")
	if err := setupClientTemplate.Execute(&setup, data); err != nil {
		return data, err
	}
	setup.WriteString("POUDRINE_SETUP\n")
	data.SetupCommand = setup.String()
	target := struct{ Path, URL string }{
		Path: reportPath,
		URL:  strings.ReplaceAll(u.JoinPath(reportPath).String(), "'", "'\\''"),
	}
	var report strings.Builder
	if err := reportClientTemplate.Execute(&report, target); err != nil {
		return data, err
	}
	data.ReportCommand = report.String()
	return data, nil
}

func (a *Admin) index(w http.ResponseWriter, r *http.Request) {
	files, err := makeClientInstructions(a.cfg.RepositoryURL, a.cfg.PublicKey())
	if err != nil {
		adminInternal(w, err)
		return
	}
	var page bytes.Buffer
	err = repositoryPage.Execute(&page, struct {
		clientInstructions
		Stats CatalogStats
	}{files, a.cfg.Stats()})
	if err != nil {
		adminInternal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = page.WriteTo(w)
}

func (a *Admin) clientSetup(w http.ResponseWriter, r *http.Request) {
	files, err := makeClientInstructions(a.cfg.RepositoryURL, a.cfg.PublicKey())
	if err != nil {
		adminInternal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="setup-client.sh"`)
	// Keep the catalog refresh for clients using the original download.
	fmt.Fprint(w, "#!/bin/sh\nset -e\n", files.SetupCommand, "pkg update -f -r Poudrine\n")
}

func printClientSetup(o options, out io.Writer) error {
	address, err := o.advertisedRepository(0)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(o.state, "repository.pub"), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("read repository.pub after the first service start: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	const maxKeyBytes = 32 << 10
	if !info.Mode().IsRegular() || info.Size() > maxKeyBytes {
		return errors.New("expected a PEM public key file at repository.pub")
	}
	key, err := io.ReadAll(io.LimitReader(f, maxKeyBytes+1))
	if err != nil {
		return err
	}
	files, err := makeClientInstructions(address, string(key))
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, files.SetupCommand)
	return err
}
