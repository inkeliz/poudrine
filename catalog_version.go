package poudrine

import (
	"cmp"
	"strconv"
	"strings"
)

// FreeBSD port versions compare epoch first, then components, then PORTREVISION.
// Components have a number, optional letter/stage, and optional patch level.
func comparePkgVersions(a, b string) int {
	split := func(s string) (string, uint64, uint64) {
		if i := strings.LastIndexByte(s, '-'); i >= 0 {
			s = s[i+1:]
		}
		v, e, _ := strings.Cut(s, ",")
		v, r, _ := strings.Cut(v, "_")
		epoch, _ := strconv.ParseUint(e, 10, 64)
		rev, _ := strconv.ParseUint(r, 10, 64)
		return strings.ToLower(v), epoch, rev
	}
	a, ea, ra := split(a)
	b, eb, rb := split(b)
	if c := cmp.Compare(ea, eb); c != 0 {
		return c
	}
	for a != "" || b != "" {
		var ca, cb [3]int64
		blockA := a == "" || a[0] == '+'
		blockB := b == "" || b[0] == '+'
		if blockA && blockB {
			a = strings.TrimPrefix(a, "+")
			b = strings.TrimPrefix(b, "+")
			continue
		}
		if !blockA {
			ca, a = versionComponent(a)
		}
		if !blockB {
			cb, b = versionComponent(b)
		}
		for i := range ca {
			if c := cmp.Compare(ca[i], cb[i]); c != 0 {
				return c
			}
		}
	}
	return cmp.Compare(ra, rb)
}

func versionComponent(s string) (c [3]int64, rest string) {
	digit := func(b byte) bool { return b >= '0' && b <= '9' }
	letter := func(b byte) bool { return b >= 'a' && b <= 'z' }
	number := func() int64 {
		i := 0
		for i < len(s) && digit(s[i]) {
			i++
		}
		n, _ := strconv.ParseInt(s[:i], 10, 64)
		s = s[i:]
		return n
	}
	c[0] = -1
	if digit(s[0]) {
		c[0] = number()
	} else if s[0] == '*' {
		_, rest, found := strings.Cut(s, "+")
		if found {
			rest = "+" + rest
		}
		return [3]int64{-2, 0, 0}, rest
	}
	if len(s) > 0 && letter(s[0]) {
		n := 1
		for n < len(s) && letter(s[n]) {
			n++
		}
		word := s[:n]
		switch word {
		case "pl", "snap", "alpha", "beta", "pre", "rc":
			if c[0] >= 0 {
				return c, s
			} // pkg inserts a separator before these stages.
		}
		if word != "pl" {
			c[1] = int64(word[0]-'a') + 2
		}
		s = s[n:]
		c[2] = -1
		if len(s) > 0 && digit(s[0]) {
			c[2] = number()
		}
	}
	for len(s) > 0 && !letter(s[0]) && !digit(s[0]) && s[0] != '*' && s[0] != '+' {
		s = s[1:]
	}
	return c, s
}
