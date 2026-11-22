package catalog

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"

	"github.com/inkeliz/poudrine/internal/packageinfo"
)

func TestNativeBootstrapSignature(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c := &Catalog{Key: key}
	pkg := packageinfo.Package{Name: "pkg", Path: "All/pkg-1.pkg", Size: 3, Sum: "2$kgygnaah7wxsgn1wkuic4j78zq8dicmx53picmma99ogmkbd7k5nhuxr5xxemz6yzjab15oor3tjt7nupj8mh764y7kddbne7qw9agn"}
	c.Current.Store(&Snapshot{bootstrapPath: pkg.Path, ByPath: map[string]packageinfo.Package{pkg.Path: pkg}})
	for _, p := range []string{"Latest/pkg.pkg", "Latest/pkg.pkg.pubkeysig", "Latest/pkg.txz", "Latest/pkg.txz.pubkeysig"} {
		got, ok := c.Package(p)
		if !ok || got != pkg {
			t.Fatalf("%s: %+v", p, got)
		}
	}
	sig, err := c.BootstrapSignature(strings.NewReader("foo"), pkg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != key.Size() {
		t.Fatal("bootstrap signature should be raw RSA")
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, PkgSignatureDigest([]byte("foo")), sig); err != nil {
		t.Fatal(err)
	}
	if _, err = c.BootstrapSignature(strings.NewReader("bad"), pkg); err == nil {
		t.Fatal("signed unverified archive")
	}
}
