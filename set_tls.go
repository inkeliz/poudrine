package poudrine

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// setTLS installs a certificate selection in the same file used by web setup.
// PEM material stays in the original files with administrator-chosen permissions.
func setTLS(args []string) error {
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
		if err := checkRootConfigPath(path); err != nil {
			return err
		}
	}
	c, revision, err := loadConfiguration(path)
	if errors.Is(err, os.ErrNotExist) {
		c = defaultConfiguration()
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
	if _, err := saveConfiguration(path, revision, data); err != nil {
		return err
	}
	fmt.Printf("Website HTTPS settings saved to %s.\n", path)
	return nil
}
