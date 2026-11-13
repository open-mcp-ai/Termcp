package encoding

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// UnescapeJSON turns the \uXXXX escapes a JSON producer left in a string into the
// UTF-8 the escapes stand for.
//
// WHY THIS IS NEEDED AT ALL: the escapes are not termcp's. Chrome's DevTools
// protocol serialises every non-ASCII character that way, so a page title in
// Chinese arrives as `\u4e2d\u6587` — twelve bytes of escape for two characters,
// and unreadable without decoding them first. Passing the frame through verbatim
// is the honest thing for a byte channel to do, and it is exactly what made the
// browser door worse than a client that parses JSON and re-serialises it: the
// model paid double the tokens to read something it then had to decode in its
// head.
//
// ONLY \u ESCAPES ARE TOUCHED. Every other escape (\n, \t, \", \\) stays as it is,
// because the caller is reading a frame, not a string, and rewriting its
// whitespace would change what the peer said. A \\uXXXX (an escaped backslash
// followed by the letters uXXXX) is content, not an escape, and is left alone.
//
// Surrogate pairs are combined. A lone surrogate cannot be represented in UTF-8,
// so it is left as the escape it was rather than replaced with U+FFFD, which would
// silently change the peer's frame.
func UnescapeJSON(s string) string {
	if !strings.Contains(s, `\u`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte(c)
			i++
			continue
		}
		// A doubled backslash is one literal backslash: whatever follows it is
		// content, so the `\u` inside `\\u4e2d` must not be decoded.
		if s[i+1] == '\\' {
			b.WriteString(`\\`)
			i += 2
			continue
		}
		r, size, ok := decodeUEscape(s[i:])
		if !ok {
			b.WriteByte(c)
			i++
			continue
		}
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

// decodeUEscape decodes the escape at the start of s, which must begin with \u.
// size is how many bytes of s the escape occupied: 6, or 12 for a surrogate pair.
func decodeUEscape(s string) (rune, int, bool) {
	if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
		return 0, 0, false
	}
	v, ok := hex4(s[2:6])
	if !ok {
		return 0, 0, false
	}
	if !utf16.IsSurrogate(rune(v)) {
		if !utf8.ValidRune(rune(v)) {
			return 0, 0, false
		}
		return rune(v), 6, true
	}
	// A high surrogate is only half a character; the low half follows as its own
	// escape. A low surrogate with no high one, or a high one with no low, is not
	// decodable and stays as written.
	if v < 0xD800 || v > 0xDBFF || len(s) < 12 || s[6] != '\\' || s[7] != 'u' {
		return 0, 0, false
	}
	lo, ok := hex4(s[8:12])
	if !ok || lo < 0xDC00 || lo > 0xDFFF {
		return 0, 0, false
	}
	return utf16.DecodeRune(rune(v), rune(lo)), 12, true
}

func hex4(s string) (uint32, bool) {
	if len(s) < 4 {
		return 0, false
	}
	var v uint32
	for i := 0; i < 4; i++ {
		d, ok := hexDigit(s[i])
		if !ok {
			return 0, false
		}
		v = v<<4 | uint32(d)
	}
	return v, true
}

// TrailingEscapeStart reports where in s an escape begins that s cuts short — a
// \uXXXX with fewer than four hex digits, or a lone backslash at the very end — or
// -1 when s does not end inside one.
//
// WHY A CALLER WANTS THIS. `read --at` serves a frame a slice at a time, and the
// offset it hands back for the next slice is a byte count it chose. A count that
// lands between the \u and its hex digits gives the next reader the leftovers: the
// seam reads "17e讯云" instead of "腾讯云". Backing off to this index is the same job
// the UTF-8 boundary check does for a character cut in half — and it belongs here,
// next to the code that knows what an escape is, rather than being counted a second
// time by whoever is doing the slicing.
func TrailingEscapeStart(s string) int {
	i := strings.LastIndexByte(s, '\\')
	if i < 0 {
		return -1
	}
	tail := s[i:]
	// \uXXXX is six bytes; a longer tail holds a whole one and is not a cut.
	if len(tail) >= 6 {
		return -1
	}
	// A lone backslash is the first byte of an escape that has not arrived yet;
	// anything else shorter than six bytes has to at least look like the start of one.
	if len(tail) > 1 && !strings.HasPrefix(tail, `\u`) {
		return -1
	}
	// A backslash in an even-length run is itself escaped, so what follows it is
	// content: "\\u4e2d" is six characters, not one.
	run := 0
	for j := i; j >= 0 && s[j] == '\\'; j-- {
		run++
	}
	if run%2 == 0 {
		return -1
	}
	return i
}
