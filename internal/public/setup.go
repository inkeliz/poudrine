package public

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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	texttemplate "text/template"

	"github.com/inkeliz/poudrine/internal/config"
	"github.com/inkeliz/poudrine/internal/constanttime"
	"github.com/inkeliz/poudrine/internal/routes"
	poudrineweb "github.com/inkeliz/poudrine/internal/web/poudrine"
)

var setupClientTemplate = texttemplate.Must(texttemplate.ParseFS(poudrineweb.Files, "setup-client.sh.tmpl"))

var reportClientTemplate = texttemplate.Must(texttemplate.ParseFS(poudrineweb.Files, "report-inventory.sh.tmpl"))

var RepositoryPage = template.Must(template.ParseFS(poudrineweb.Files, "index.html"))

type ClientInstructions struct {
	RepositoryURL, RepoConfig, PublicKey, Fingerprint string
	SetupCommand, ReportCommand                       string
}

func MakeClientInstructions(address, publicKey string) (ClientInstructions, error) {
	var data ClientInstructions
	u, err := config.WebURL(address)
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
	fingerprint := sha256.Sum256([]byte(data.PublicKey))
	data.Fingerprint = constanttime.EncodeHex(fingerprint[:])
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
		Path: routes.ReportInventory.Path,
		URL:  strings.ReplaceAll(u.JoinPath(routes.ReportInventory.Path).String(), "'", "'\\''"),
	}
	var report strings.Builder
	if err := reportClientTemplate.Execute(&report, target); err != nil {
		return data, err
	}
	data.ReportCommand = report.String()
	return data, nil
}

func PrintClientSetup(o config.Options, out io.Writer) error {
	address, err := o.AdvertisedRepository(0)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(o.State, "repository.pub"), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
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
	files, err := MakeClientInstructions(address, string(key))
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, files.SetupCommand)
	return err
}
