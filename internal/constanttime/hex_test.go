package constanttime

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestDecodeHexEveryBytePair(t *testing.T) {
	var got [1]byte
	for hi := range 256 {
		for lo := range 256 {
			src := string([]byte{byte(hi), byte(lo)})
			want, err := hex.DecodeString(src)
			valid := DecodeHex(got[:], src)
			if (valid == 1) != (err == nil) || err == nil && got[0] != want[0] {
				t.Fatalf("decode %x: %x/%d, want %x/%v", src, got, valid, want, err)
			}
			if ValidHex(src) != (err == nil) {
				t.Fatalf("validation differs for %x", src)
			}
		}
	}
}

func TestHexEqualityAndInvalidInput(t *testing.T) {
	const input = "0123456789abcdef"
	decoded, _ := hex.DecodeString(input)
	if !HexEqual(input, strings.ToUpper(input)) || !HexMatches(strings.ToUpper(input), decoded) {
		t.Fatal("case insensitive comparison failed")
	}
	for i := range len(input) {
		bad := input[:i] + "!" + input[i+1:]
		if HexEqual(bad, bad) || HexEqual(input, bad) || HexMatches(bad, decoded) {
			t.Fatalf("invalid encoding accepted at %d", i)
		}
		other := input[:i] + "f" + input[i+1:]
		if i != len(input)-1 && (HexEqual(input, other) || HexMatches(other, decoded)) {
			t.Fatalf("mismatch accepted at %d", i)
		}
	}
	var dst [2]byte
	if DecodeHex(dst[:], "!a12") != 0 || dst[1] != 0x12 {
		t.Fatal("decoder stopped at invalid input")
	}
	if DecodeHex(dst[:], "123") != 0 || DecodeHex(dst[:], "12") != 0 || HexEqual("1", "1") || HexMatches("1", []byte{1}) {
		t.Fatal("invalid length accepted")
	}
}
