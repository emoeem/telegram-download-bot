// Package license implements the offline commercial licence that gates this
// build: a signed, device-bound, optionally expiring key that the vendor issues
// from their own machine and the customer types once.
//
// The design constraint that shapes everything here is that the *issuer* has no
// server. There is no activation endpoint to call, so legitimacy has to be
// provable from the key alone. That means asymmetric signatures: the binary
// carries a public key, the issuer keeps the private half, and no amount of
// disassembly gives a customer the ability to mint their own key.
package license

import "strings"

// alphabet is Crockford base32: the ten digits plus 22 letters, with I, L, O
// and U removed.
//
// Those four are removed because they are the characters humans misread when
// copying a key off a screen: I/1, L/1, O/0, and U/V. A licence key gets typed
// by hand or pasted out of a chat window, so an alphabet that cannot produce an
// ambiguous glyph costs nothing and removes a whole class of support tickets.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// decodeMap canonicalises one input byte to its 5-bit value. I, L and O are
// folded onto 1 and 0 rather than rejected: a customer who read "0" as "O"
// should still activate, and the error message they would otherwise get
// ("invalid key") tells them nothing about the typo.
var decodeMap [256]int8

func init() {
	for i := range decodeMap {
		decodeMap[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		c := alphabet[i]
		decodeMap[c] = int8(i)
	}
	// Lower case is the same key. A key pasted from an email that auto-
	// lowercased it is not a different key.
	for i := 0; i < len(alphabet); i++ {
		c := alphabet[i]
		switch c {
		case '0':
			decodeMap['O'] = int8(i)
			decodeMap['o'] = int8(i)
		case '1':
			decodeMap['I'] = int8(i)
			decodeMap['i'] = int8(i)
			decodeMap['L'] = int8(i)
			decodeMap['l'] = int8(i)
		default:
			decodeMap[lower(c)] = int8(i)
		}
	}
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// encode36 renders v as exactly n Crockford characters, most significant first.
//
// Fixed width matters: the parser slices the key by position, so a field that
// could render as fewer characters would silently shift every field after it.
func encode36(v uint64, n int) string {
	buf := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		buf[i] = alphabet[v&0x1f]
		v >>= 5
	}
	return string(buf)
}

// decode32 parses exactly n Crockford characters into a uint64.
//
// It rejects anything that is not n characters of the alphabet, and it rejects
// a first character that would overflow the destination width — a 40-bit field
// whose leading digit implies bit 40 is not a valid field, it is a truncated or
// corrupted one, and accepting it would let a tampered key parse into a
// different serial than the one that was signed.
func decode32(s string, n int, bits uint) (uint64, bool) {
	if len(s) != n {
		return 0, false
	}
	var v uint64
	for i := 0; i < n; i++ {
		d := decodeMap[s[i]]
		if d < 0 {
			return 0, false
		}
		v = v<<5 | uint64(d)
	}
	if n*5 > int(bits) {
		if v>>bits != 0 {
			return 0, false
		}
	}
	return v, true
}

// normalizeKey strips the separators and case a human or a chat client may have
// added, then upper-cases what remains.
func normalizeKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '-', ' ', '\t', '\n', '\r', '_':
			continue
		}
		b.WriteByte(upper(c))
	}
	return b.String()
}

func upper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}
