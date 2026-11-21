package poudrine

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

func bootstrapSignaturePath(p string) bool {
	return p == "Latest/pkg.pkg.pubkeysig" || p == "Latest/pkg.txz.pubkeysig"
}

// FreeBSD's base-system installer reads a detached, unframed RSA signature
// over the hex SHA256 of the pkg archive. The catalog may authenticate the
// same archive using a different hash, so compute both in a single pass.
func (c *Catalog) bootstrapSignature(r io.Reader, p CatalogPackage) ([]byte, error) {
	if p.Name != "pkg" {
		return nil, fmt.Errorf("bootstrap requires the pkg package")
	}
	h := sha256.New()
	if err := verifyPackage(r, h, p); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(hex.EncodeToString(h.Sum(nil))))
	return rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
}
