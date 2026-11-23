// Package constanttime provides content-independent operations on encoded
// digests. Input lengths and the final validity/equality result are public.
package constanttime

import "crypto/subtle"

// nibble uses arithmetic masks, without data-indexed tables or branches.
func nibble(c byte) (value, valid uint32) {
	digit := uint32(c) - '0'
	letter := uint32(c|0x20) - 'a'
	digitOK := 1 ^ ((digit | (9 - digit)) >> 31)
	letterOK := 1 ^ ((letter | (5 - letter)) >> 31)
	return (digit & (0 - digitOK)) | ((letter + 10) & (0 - letterOK)), digitOK | letterOK
}

// DecodeHex returns 1 for valid hexadecimal input and 0 otherwise. It accepts
// either case, visits every input byte even on invalid input, and writes every
// output byte. The destination must have exactly half the input length. Its
// contents are unspecified on failure. Length checks are not constant time.
func DecodeHex(dst []byte, src string) int {
	if len(src)%2 != 0 || len(dst) != len(src)/2 {
		return 0
	}
	valid := uint32(1)
	for i := range dst {
		hi, hiOK := nibble(src[2*i])
		lo, loOK := nibble(src[2*i+1])
		dst[i] = byte(hi<<4 | lo)
		valid &= hiOK & loOK
	}
	return int(valid)
}

func ValidHex(src string) bool {
	valid := uint32(1)
	for i := range len(src) {
		_, ok := nibble(src[i])
		valid &= ok
	}
	return valid == 1 && len(src)%2 == 0
}

// HexEqual compares two hex strings without content-dependent decoding or
// comparison. Invalid encodings never compare equal, including to themselves.
func HexEqual(a, b string) bool {
	if len(a) != len(b) || len(a)%2 != 0 {
		return false
	}
	left, right := make([]byte, len(a)/2), make([]byte, len(b)/2)
	validA := DecodeHex(left, a)
	validB := DecodeHex(right, b)
	return validA&validB&subtle.ConstantTimeCompare(left, right) == 1
}

// HexMatches compares hex input directly with a binary digest, avoiding an
// intermediate hex encoding (and its data-indexed alphabet lookup).
func HexMatches(src string, digest []byte) bool {
	if len(src)%2 != 0 || len(src)/2 != len(digest) {
		return false
	}
	decoded := make([]byte, len(digest))
	valid := DecodeHex(decoded, src)
	return valid&subtle.ConstantTimeCompare(decoded, digest) == 1
}

// EncodeHex returns lowercase hex without a data-indexed alphabet lookup.
func EncodeHex(src []byte) string {
	out := make([]byte, len(src)*2)
	for i, b := range src {
		hi, lo := uint32(b>>4), uint32(b&15)
		out[2*i] = byte(hi + '0' + ((9-hi)>>31)*39)
		out[2*i+1] = byte(lo + '0' + ((9-lo)>>31)*39)
	}
	return string(out)
}
