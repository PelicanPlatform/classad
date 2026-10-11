package classad

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ClassAd strings are bytes, as in libclassad, and need not be valid UTF-8 (a
// string literal keeps the bytes it was written with). The standard library's
// rune-wise case functions replace each byte that is not part of valid UTF-8
// with U+FFFD; these variants keep such a byte as it is, and treat it as equal
// only to the same byte. On valid UTF-8 they are the standard functions.

// toLowerKeepBytes is strings.ToLower, keeping bytes that are not valid UTF-8.
func toLowerKeepBytes(s string) string {
	if utf8.ValidString(s) {
		return strings.ToLower(s)
	}
	return mapKeepBytes(s, unicode.ToLower)
}

// toUpperKeepBytes is strings.ToUpper, keeping bytes that are not valid UTF-8.
func toUpperKeepBytes(s string) string {
	if utf8.ValidString(s) {
		return strings.ToUpper(s)
	}
	return mapKeepBytes(s, unicode.ToUpper)
}

func mapKeepBytes(s string, f func(rune) rune) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			b.WriteByte(s[i])
		} else {
			b.WriteRune(f(r))
		}
		i += n
	}
	return b.String()
}

// equalFoldKeepBytes is strings.EqualFold, except that a byte that is not part
// of valid UTF-8 matches only the same byte.
func equalFoldKeepBytes(s, t string) bool {
	if utf8.ValidString(s) && utf8.ValidString(t) {
		return strings.EqualFold(s, t)
	}
	for s != "" && t != "" {
		r1, n1 := utf8.DecodeRuneInString(s)
		r2, n2 := utf8.DecodeRuneInString(t)
		bad1, bad2 := r1 == utf8.RuneError && n1 == 1, r2 == utf8.RuneError && n2 == 1
		if bad1 || bad2 {
			if !bad1 || !bad2 || s[0] != t[0] {
				return false
			}
		} else if !strings.EqualFold(s[:n1], t[:n2]) {
			return false
		}
		s, t = s[n1:], t[n2:]
	}
	return s == t
}
