package constanttime

import (
	"bytes"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestCodecsMatchStandardEncodings(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, tc := range []struct {
		name   string
		codec  *Encoding
		encode func([]byte) string
	}{
		{"base32", Base32, base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString},
		{"pkg base32", NewBase32("ybndrfg8ejkmcpqxot1uwisza345h769", false), base32.NewEncoding("ybndrfg8ejkmcpqxot1uwisza345h769").WithPadding(base32.NoPadding).EncodeToString},
		{"base64url", RawURLEncoding, base64.RawURLEncoding.EncodeToString},
		{"base64", RawStdEncoding, base64.RawStdEncoding.EncodeToString},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for size := range 257 {
				src := make([]byte, size)
				for i := range src {
					src[i] = byte(rng.Uint32())
				}
				encoded := tc.codec.EncodeToString(src)
				if encoded != tc.encode(src) {
					t.Fatalf("encoding size %d", size)
				}
				decoded, err := tc.codec.DecodeString(encoded)
				if err != nil || !bytes.Equal(src, decoded) || !tc.codec.Matches(encoded, src) {
					t.Fatalf("decoding size %d: %v", size, err)
				}
				if EncodeHex(src) != hex.EncodeToString(src) {
					t.Fatalf("hex encoding size %d", size)
				}
			}
		})
	}
}

func TestBaseCodecsRejectEveryInvalidCharacterAndTrailingBits(t *testing.T) {
	for _, codec := range []*Encoding{Base32, RawURLEncoding, RawStdEncoding, NewBase32("ybndrfg8ejkmcpqxot1uwisza345h769", true)} {
		src := []byte{1, 23, 45, 67, 89, 101, 123, 145, 167}
		encoded := codec.EncodeToString(src)
		for i := range len(encoded) {
			for c := range 256 {
				if strings.ContainsRune(codec.alphabet, rune(c)) {
					continue
				}
				bad := encoded[:i] + string([]byte{byte(c)}) + encoded[i+1:]
				if _, err := codec.DecodeString(bad); err == nil || codec.Matches(bad, src) {
					t.Fatalf("accepted invalid byte %d at %d", c, i)
				}
			}
		}
		// Every encoding uses only part of the final symbol for a one-byte input.
		one := codec.EncodeToString([]byte{0})
		bad := one[:len(one)-1] + string(codec.alphabet[1])
		if codec.little {
			bad = one[:len(one)-1] + string(codec.alphabet[8])
		}
		if _, err := codec.DecodeString(bad); err == nil {
			t.Fatal("accepted noncanonical trailing bits")
		}
		if _, err := codec.DecodeString(one[:1]); err == nil {
			t.Fatal("accepted incomplete symbol group")
		}
		out := make([]byte, len(src))
		if codec.Decode(out, "!"+encoded[1:]) != 0 || !bytes.Equal(out[1:], src[1:]) {
			t.Fatal("invalid input stopped decoding early")
		}
	}
}

func TestLegacyBase32BitOrder(t *testing.T) {
	codec := NewBase32("ybndrfg8ejkmcpqxot1uwisza345h769", true)
	for size := range 129 {
		src := make([]byte, size)
		for i := range src {
			src[i] = byte(i*97 + 13)
		}
		// Independent bit-by-bit oracle for pkg's low-bit-first representation.
		want := make([]byte, codec.EncodedLen(size))
		for i := range want {
			value := 0
			for bit := range 5 {
				n := i*5 + bit
				if n < size*8 {
					value |= int(src[n/8]>>(n%8)&1) << bit
				}
			}
			want[i] = codec.alphabet[value]
		}
		if codec.EncodeToString(src) != string(want) || !codec.Matches(string(want), src) {
			t.Fatalf("legacy bit order size %d", size)
		}
	}
}
