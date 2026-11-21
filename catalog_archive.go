package poudrine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const maxCatalogBytes int64 = 512 << 20

func catalogKey(dir string) (*rsa.PrivateKey, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	name := filepath.Join(dir, "repository.key")
	raw, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		key, err := rsa.GenerateKey(rand.Reader, 3072)
		if err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(dir, ".repository-key-")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		err = pem.Encode(tmp, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
		if err = os.Link(tmp.Name(), name); err == nil {
			return key, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		return catalogKey(dir)
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("invalid repository signing key")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// Modern pkg signs the ASCII hexadecimal SHA256 of the member using RSA-SHA256.
func pkgSignatureDigest(member []byte) []byte {
	hash := sha256.Sum256(member)
	signed := sha256.Sum256([]byte(hex.EncodeToString(hash[:])))
	return signed[:]
}

func signedPkg(key *rsa.PrivateKey, member string, content []byte) ([]byte, error) {
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, pkgSignatureDigest(content))
	if err != nil {
		return nil, err
	}
	// pkg's pubkey verifier strips exactly one trailing NUL byte.
	sig = append(sig, 0)
	var out bytes.Buffer
	z, err := zstd.NewWriter(&out, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	t := tar.NewWriter(z)
	entries := []struct {
		name string
		b    []byte
	}{{"signature", sig}, {member, content}}
	for _, e := range entries {
		h := tar.Header{Name: e.name, Size: int64(len(e.b)), Mode: 0644, Typeflag: tar.TypeReg}
		if err = t.WriteHeader(&h); err != nil {
			return nil, err
		}
		if _, err = t.Write(e.b); err != nil {
			return nil, err
		}
	}
	if err = t.Close(); err != nil {
		return nil, err
	}
	if err = z.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func decompressArchive(r io.Reader) (io.ReadCloser, error) {
	buf := bufio.NewReader(r)
	magic, err := buf.Peek(6)
	if err != nil {
		return nil, err
	}
	switch {
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		z, err := zstd.NewReader(buf, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(256<<20))
		if err != nil {
			return nil, err
		}
		return z.IOReadCloser(), nil
	case bytes.HasPrefix(magic, []byte{0xfd, '7', 'z', 'X', 'Z', 0}):
		x, err := (xz.ReaderConfig{DictCap: 128 << 20}).NewReader(buf)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(x), nil
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		return gzip.NewReader(buf)
	case bytes.HasPrefix(magic, []byte("BZh")):
		return io.NopCloser(bzip2.NewReader(buf)), nil
	}
	return io.NopCloser(buf), nil
}

// unpackCatalog reads only named regular members, never archive paths on disk.
func unpackCatalog(r io.Reader, member string) (map[string][]byte, error) {
	src, err := decompressArchive(r)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	const overhead = 1 << 20
	limited := &io.LimitedReader{R: src, N: maxCatalogBytes + overhead}
	t := tar.NewReader(limited)
	m := make(map[string][]byte)
	for {
		hdr, err := t.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		switch name {
		case member, member + ".pub", member + ".sig", "signature":
		default:
			continue
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size > maxCatalogBytes || name != member && hdr.Size > 65536 {
			return nil, fmt.Errorf("invalid catalog member %s", name)
		}
		if _, duplicate := m[name]; duplicate {
			return nil, fmt.Errorf("duplicate catalog member %s", name)
		}
		content, err := io.ReadAll(t)
		if err != nil {
			return nil, err
		}
		m[name] = content
	}
	if limited.N == 0 || len(m[member]) == 0 {
		return nil, fmt.Errorf("oversized catalog or missing %s", member)
	}
	return m, nil
}

var fpDigest = regexp.MustCompile(`(?im)^\s*"?fingerprint"?\s*[:=]\s*"([a-f0-9]{64})"`)
var fpFunction = regexp.MustCompile(`(?im)^\s*"?function"?\s*[:=]\s*"sha256"`)

func findFingerprint(dir, hash string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return false, err
		}
		match := fpDigest.FindSubmatch(b)
		if !fpFunction.Match(b) || match == nil {
			return false, fmt.Errorf("invalid pkg fingerprint %q", e.Name())
		}
		if strings.EqualFold(string(match[1]), hash) {
			return true, nil
		}
	}
	return false, nil
}

func checkUpstreamFingerprint(trust, id string) error {
	denied, err := findFingerprint(filepath.Join(trust, "revoked"), id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if denied {
		return errors.New("revoked upstream signing key")
	}
	trusted, err := findFingerprint(filepath.Join(trust, "trusted"), id)
	if err != nil {
		return err
	}
	if !trusted {
		return errors.New("untrusted upstream signing key")
	}
	return nil
}

func verifyCatalog(m map[string][]byte, member, trust string) error {
	pub := m[member+".pub"]
	sig := m[member+".sig"]
	if len(pub) == 0 || len(sig) == 0 {
		return errors.New("missing upstream catalog signature")
	}
	id := fmt.Sprintf("%x", sha256.Sum256(pub))
	if err := checkUpstreamFingerprint(trust, id); err != nil {
		return err
	}
	block, _ := pem.Decode(pub)
	if block == nil {
		return errors.New("invalid upstream RSA public key")
	}
	pk, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return err
	}
	rsaKey, ok := pk.(*rsa.PublicKey)
	if !ok {
		return errors.New("expected RSA public key")
	}
	return rsa.VerifyPKCS1v15(rsaKey, crypto.SHA256, pkgSignatureDigest(m[member]), sig)
}
