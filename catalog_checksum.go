package poudrine

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/blake2s"
)

const pkgAlphabet = "ybndrfg8ejkmcpqxot1uwisza345h769"

var pkgBase32 = base32.NewEncoding(pkgAlphabet).WithPadding(base32.NoPadding)

func checksumParts(sum string) (kind, value string) {
	kind, value, ok := strings.Cut(sum, "$")
	if !ok {
		return "1", sum
	}
	return kind, value
}

// pkg catalogs use both unprefixed SHA256 and typed checksums.
func packageHasher(sum string) (hash.Hash, error) {
	kind, value := checksumParts(sum)
	var h hash.Hash
	switch kind {
	case "0", "1", "12":
		h = sha256.New()
	case "2", "10", "13":
		h, _ = blake2b.New512(nil)
	case "5", "11", "14":
		h, _ = blake2s.New256(nil)
	case "7", "8":
		h = sha512.New()
	default:
		return nil, fmt.Errorf("unknown pkg checksum type %q", kind)
	}
	switch kind {
	case "1", "8", "10", "11":
		if len(value) != hex.EncodedLen(h.Size()) || !isHex(value) {
			return nil, fmt.Errorf("invalid hex package checksum")
		}
	default:
		if len(value) != (h.Size()*8+4)/5 || strings.Trim(value, pkgAlphabet) != "" {
			return nil, fmt.Errorf("invalid base32 package checksum")
		}
	}
	return h, nil
}
func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func packageHashMatches(sum string, h hash.Hash) bool {
	kind, value := checksumParts(sum)
	digest := h.Sum(nil)
	switch kind {
	case "1", "8", "10", "11":
		return strings.EqualFold(value, hex.EncodeToString(digest))
	case "12", "13", "14":
		return value == pkgBase32.EncodeToString(digest)
	}
	// pkg's historical BASE32 codes consume bits from the low end first.
	var s strings.Builder
	var bits uint
	var pending uint32
	for _, b := range digest {
		pending |= uint32(b) << bits
		bits += 8
		for bits >= 5 {
			s.WriteByte(pkgAlphabet[pending&31])
			pending >>= 5
			bits -= 5
		}
	}
	if bits > 0 {
		s.WriteByte(pkgAlphabet[pending&31])
	}
	return value == s.String()
}
