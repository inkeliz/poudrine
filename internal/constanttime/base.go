package constanttime

import (
	"crypto/subtle"
	"errors"
)

var errEncoding = errors.New("invalid encoded data")

// Encoding is an unpadded, canonical base32 or base64 encoding. Its alphabet
// and bit order are public. Every alphabet selection scans all entries with
// arithmetic masks; no memory address depends on an input character or digest.
// As with DecodeHex, lengths and the final validity result are public.
type Encoding struct {
	alphabet string
	bits     uint
	little   bool
}

var RawURLEncoding = &Encoding{alphabet: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", bits: 6}
var RawStdEncoding = &Encoding{alphabet: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", bits: 6}
var Base32 = NewBase32("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", false)

func NewBase32(alphabet string, littleEndian bool) *Encoding {
	if len(alphabet) != 32 {
		panic("base32 alphabet must have 32 characters")
	}
	for i := range len(alphabet) {
		for j := 0; j < i; j++ {
			if alphabet[i] == alphabet[j] {
				panic("duplicate base32 alphabet character")
			}
		}
	}
	return &Encoding{alphabet: alphabet, bits: 5, little: littleEndian}
}

func (e *Encoding) EncodedLen(n int) int { return (n*8 + int(e.bits) - 1) / int(e.bits) }
func (e *Encoding) DecodedLen(n int) int { return n/8*int(e.bits) + n%8*int(e.bits)/8 }
func (e *Encoding) Strict() *Encoding    { return e }

func (e *Encoding) symbol(value byte) byte {
	var out byte
	for i := range len(e.alphabet) {
		mask := byte(0 - subtle.ConstantTimeByteEq(byte(i), value))
		out |= e.alphabet[i] & mask
	}
	return out
}

func (e *Encoding) value(c byte) (uint32, int) {
	var out uint32
	valid := 0
	for i := range len(e.alphabet) {
		match := subtle.ConstantTimeByteEq(e.alphabet[i], c)
		out |= uint32(i) & (0 - uint32(match))
		valid |= match
	}
	return out, valid
}

func (e *Encoding) EncodeToString(src []byte) string {
	out := make([]byte, e.EncodedLen(len(src)))
	var pending uint32
	var bits uint
	pos := 0
	mask := uint32(1<<e.bits) - 1
	for _, b := range src {
		if e.little {
			pending |= uint32(b) << bits
		} else {
			pending = pending<<8 | uint32(b)
		}
		bits += 8
		for bits >= e.bits {
			bits -= e.bits
			value := pending >> bits & mask
			if e.little {
				value = pending & mask
				pending >>= e.bits
			}
			out[pos] = e.symbol(byte(value))
			pos++
		}
	}
	if bits > 0 {
		value := pending << (e.bits - bits) & mask
		if e.little {
			value = pending & mask
		}
		out[pos] = e.symbol(byte(value))
	}
	return string(out)
}

// Decode processes all input characters before returning validity. Invalid
// input still writes the entire destination; discard it unless the result is 1.
func (e *Encoding) Decode(dst []byte, src string) int {
	if len(dst) != e.DecodedLen(len(src)) || len(src) != e.EncodedLen(len(dst)) {
		return 0
	}
	var pending uint32
	var bits uint
	pos, valid := 0, 1
	for i := range len(src) {
		value, ok := e.value(src[i])
		valid &= ok
		if e.little {
			pending |= value << bits
		} else {
			pending = pending<<e.bits | value
		}
		bits += e.bits
		if bits >= 8 {
			bits -= 8
			b := byte(pending >> bits)
			if e.little {
				b = byte(pending)
				pending >>= 8
			}
			dst[pos] = b
			pos++
		}
	}
	unused := pending & uint32((1<<bits)-1)
	if e.little {
		unused = pending
	}
	return valid & subtle.ConstantTimeEq(int32(unused), 0)
}

func (e *Encoding) DecodeString(src string) ([]byte, error) {
	out := make([]byte, e.DecodedLen(len(src)))
	if e.Decode(out, src) != 1 {
		return nil, errEncoding
	}
	return out, nil
}

func (e *Encoding) Matches(src string, digest []byte) bool {
	if len(src) != e.EncodedLen(len(digest)) {
		return false
	}
	decoded := make([]byte, len(digest))
	valid := e.Decode(decoded, src)
	return valid&subtle.ConstantTimeCompare(decoded, digest) == 1
}
