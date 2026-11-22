package config

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/inkeliz/poudrine/internal/fsutil"
)

// setTLS installs a certificate selection in the same file used by web setup.
// PEM material stays in the original files with administrator-chosen permissions.
func SetTLS(args []string) error {
	flags := flag.NewFlagSet("poudrine set-tls", flag.ContinueOnError)
	config := flags.String("config", defaultConfigPath, "JSON configuration file")
	cert := flags.String("tls-cert", "", "PEM certificate file, including intermediates")
	key := flags.String("tls-key", "", "PEM private key matching the certificate")
	selfSigned := flags.Bool("self-signed", false, "use the server's automatically generated certificate")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: poudrine set-tls [-config path] -tls-cert certificate.pem -tls-key private.pem")
		fmt.Fprintln(flags.Output(), "       poudrine set-tls [-config path] -self-signed")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *selfSigned && (*cert != "" || *key != "") {
		return errors.New("choose either -self-signed or the custom certificate and key")
	}
	if !*selfSigned && (*cert == "" || *key == "") {
		return errors.New("provide both -tls-cert and -tls-key, or select -self-signed")
	}
	path, err := filepath.Abs(*config)
	if err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := CheckRootConfigPath(path); err != nil {
			return err
		}
	}
	c, revision, err := LoadConfiguration(path)
	if errors.Is(err, os.ErrNotExist) {
		c = DefaultConfiguration()
	} else if err != nil {
		return err
	}
	c.WebTLS = true
	c.TLSCert, c.TLSKey = "", ""
	if !*selfSigned {
		c.TLSCert, err = filepath.Abs(*cert)
		if err != nil {
			return err
		}
		c.TLSKey, err = filepath.Abs(*key)
		if err != nil {
			return err
		}
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if _, err := SaveConfiguration(path, revision, data); err != nil {
		return err
	}
	fmt.Printf("Website HTTPS settings saved to %s.\n", path)
	return nil
}

// A path selected from the settings page can point to any file readable by
// the web account. Open without blocking on a FIFO and bound the PEM input.
func readWebPEM(path string) ([]byte, error) {
	const limit = 4 << 20
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("expected a regular PEM file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(data) > limit {
		return nil, errors.New("PEM file exceeds 4 MiB")
	}
	return data, err
}

func LoadWebsiteKeyPair(certFile, keyFile string) (tls.Certificate, error) {
	certPEM, err := readWebPEM(certFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls_cert: %w", err)
	}
	keyPEM, err := readWebPEM(keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls_key: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls_cert: check the certificate and matching tls_key: %w", err)
	}
	return pair, nil
}

func ValidateWebTLS(ctx context.Context, o Options, supervised bool) error {
	if !o.WebTLS || o.TLSCert == "" {
		return nil
	}
	if !supervised || os.Geteuid() != 0 {
		_, err := LoadWebsiteKeyPair(o.TLSCert, o.TLSKey)
		return err
	}
	// The root supervisor checks access as the selected user, including
	// when a single web update changes both the files and the account.
	account, err := user.Lookup(o.ServiceUser)
	if err != nil {
		return fmt.Errorf("service_user: %w", err)
	}
	cred, err := ServiceCredential(account)
	if err != nil {
		return fmt.Errorf("service_user: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := ChildProcess(ctx, exe, cred, "check-tls-pair", o.TLSCert, o.TLSKey)
	cmd.Stderr = nil // Output attaches the child's diagnostic to an ExitError.
	if _, err := cmd.Output(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) != 0 {
			return errors.New(strings.TrimSpace(string(exit.Stderr)))
		}
		return fmt.Errorf("tls_cert: could not verify the certificate as %s: %w", o.ServiceUser, err)
	}
	return nil
}

func AutomaticTLSFiles(stateDir string) (cert, key string) {
	return filepath.Join(stateDir, "web-tls.crt"), filepath.Join(stateDir, "web-tls.key")
}

// The caller holds the state directory's service lock throughout startup.
func WebTLSConfig(stateDir, listenAddr, websiteURL, certFile, keyFile string) (*tls.Config, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("set both the website certificate and private key paths")
	}
	if certFile == "" {
		certFile, keyFile = AutomaticTLSFiles(stateDir)
		names, err := webCertNames(listenAddr, websiteURL)
		if err != nil {
			return nil, err
		}
		if err := prepareWebCertificate(certFile, keyFile, names); err != nil {
			return nil, fmt.Errorf("prepare website certificate: %w", err)
		}
	}
	pair, err := LoadWebsiteKeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}, nil
}

func webCertNames(listenAddr, websiteURL string) ([]string, error) {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return nil, err
	}
	names := []string{"localhost", "127.0.0.1", "::1"}
	add := func(host string) {
		if ip, err := netip.ParseAddr(host); err == nil {
			ip = ip.WithZone("").Unmap()
			if !ip.IsUnspecified() && !ip.IsMulticast() {
				names = append(names, ip.String())
			}
		} else if host != "" {
			names = append(names, strings.ToLower(strings.TrimSuffix(host, ".")))
		}
	}
	add(host)
	if hostname, err := os.Hostname(); err == nil {
		add(hostname)
	}
	if websiteURL != "" {
		u, err := WebURL(websiteURL)
		if err != nil {
			return nil, err
		}
		add(u.Hostname())
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			if n, ok := addr.(*net.IPNet); ok {
				add(n.IP.String())
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func prepareWebCertificate(certFile, keyFile string, names []string) error {
	certPEM, err := readWebPEM(certFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	certExists := err == nil
	keyPEM, err := readWebPEM(keyFile)
	var key *ecdsa.PrivateKey
	switch {
	case errors.Is(err, os.ErrNotExist):
		if certExists {
			return fmt.Errorf("%s exists without %s; restore the matching private key", certFile, keyFile)
		}
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return err
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		if err := fsutil.WriteAtomicFile(keyFile, keyPEM, 0600); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		block, _ := pem.Decode(keyPEM)
		if block == nil {
			return fmt.Errorf("invalid private key in %s", keyFile)
		}
		key, err = x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return fmt.Errorf("invalid private key in %s: %w", keyFile, err)
		}
	}
	now := time.Now()
	if certExists {
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return fmt.Errorf("invalid saved website certificate or private key: %w", err)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return err
		}
		// An interface disappearing does not invalidate a certificate. Keep
		// the same certificate until it needs a new name or renewal.
		missingName := slices.ContainsFunc(names, func(name string) bool { return leaf.VerifyHostname(name) != nil })
		if !missingName && !now.Before(leaf.NotBefore) && now.Add(30*24*time.Hour).Before(leaf.NotAfter) {
			return nil
		}
	}
	template := &x509.Certificate{
		Subject:   pkix.Name{CommonName: "Poudrine website"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(1, 0, 0),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomicFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644)
}
