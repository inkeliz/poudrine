// Package requestauth verifies the common Poudrine HTTP request signature.
package requestauth

import (
	"crypto/ed25519"
	"strconv"
	"strings"
	"time"

	"github.com/inkeliz/poudrine/internal/constanttime"
)

const Query = "QUERY"
const ClockSkewSeconds = 300

// ReportProof carries the verified signer and freshness fields for inventory replay checks.
type ReportProof struct {
	Key   ed25519.PublicKey `json:"-"`
	Nonce []byte            `json:"-"`
	Stamp int64             `json:"-"`
}

// PublicKey decodes the raw Ed25519 key carried by a self-signed request.
func PublicKey(text string) ed25519.PublicKey {
	if len(text) != constanttime.RawURLEncoding.EncodedLen(ed25519.PublicKeySize) {
		return nil
	}
	key, err := constanttime.RawURLEncoding.Strict().DecodeString(text)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(key)
}

// Verify checks the UTF-8 metadata prefix and exact body bytes before any
// decoding. The target is the escaped path received after proxy prefix removal.
// Callers enforce their own key trust and nonce replay policies.
func Verify(key ed25519.PublicKey, method, path, timestamp, nonceText, signature string, body []byte) ([]byte, int64, bool) {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") ||
		len(nonceText) != constanttime.RawURLEncoding.EncodedLen(16) ||
		len(signature) != constanttime.RawURLEncoding.EncodedLen(ed25519.SignatureSize) {
		return nil, 0, false
	}
	now := time.Now().Unix()
	stamp, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || stamp < now-ClockSkewSeconds || stamp > now+ClockSkewSeconds || len(key) != ed25519.PublicKeySize {
		return nil, 0, false
	}
	nonce, err := constanttime.RawURLEncoding.Strict().DecodeString(nonceText)
	if err != nil || len(nonce) != 16 {
		return nil, 0, false
	}
	sig, err := constanttime.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, 0, false
	}
	prefix := strings.Join([]string{"poudrine-v2", method, path, timestamp, nonceText, ""}, "\n")
	if !ed25519.Verify(key, append([]byte(prefix), body...), sig) {
		return nil, 0, false
	}
	return nonce, stamp, true
}
