package encoding

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// EncodeText encodes a byte slice as TEXT: valid UTF-8 passes through, and only
// bytes that are neither printable ASCII, common whitespace, nor part of a valid
// UTF-8 sequence are escaped as \xHH.
//
// THE ESCAPE RULE WAS ASCII-ONLY, and that made `file --action read --mode text`
// unable to return any text that was not ASCII. A file holding 中文ok — written
// correctly as eight UTF-8 bytes — read back as \xe4\xb8\xad\xe6\x96\x87ok, so
// every caller had to decode escapes to see the bytes they had just written. The
// asymmetry was the tell: write took the text at face value, read did not.
//
// Decoding runes rather than iterating bytes is what makes the difference: a rune
// that DecodeRune accepts is by definition a complete, valid sequence, so emitting
// it cannot corrupt the stream. Invalid bytes still escape, which is the property
// DecodeText depends on for a lossless round trip.
func EncodeText(data []byte) string {
	var b strings.Builder
	for i := 0; i < len(data); {
		c := data[i]
		// Printable ASCII and the three whitespace escapes we spell out.
		switch {
		case c >= 0x20 && c <= 0x7E:
			b.WriteByte(c)
			i++
			continue
		case c == '\n':
			b.WriteString("\\n")
			i++
			continue
		case c == '\r':
			b.WriteString("\\r")
			i++
			continue
		case c == '\t':
			b.WriteString("\\t")
			i++
			continue
		}
		// A multi-byte UTF-8 sequence is DATA, not an escape. DecodeRune returns
		// RuneError with size 1 for anything invalid, which falls through to the
		// \xHH branch below and keeps DecodeText a faithful inverse.
		if r, size := utf8.DecodeRune(data[i:]); r != utf8.RuneError && size > 1 {
			b.Write(data[i : i+size])
			i += size
			continue
		}
		fmt.Fprintf(&b, "\\x%02x", c)
		i++
	}
	return b.String()
}

// DecodeText decodes a text string with \xHH, \n, \r, \t escapes back to bytes.
func DecodeText(s string) []byte {
	var result []byte
	i := 0
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				result = append(result, '\n')
				i += 2
				continue
			case 'r':
				result = append(result, '\r')
				i += 2
				continue
			case 't':
				result = append(result, '\t')
				i += 2
				continue
			case 'x':
				if i+3 < len(s) {
					var b byte
					fmt.Sscanf(s[i+2:i+4], "%02x", &b)
					result = append(result, b)
					i += 4
					continue
				}
			case '\\':
				result = append(result, '\\')
				i += 2
				continue
			}
		}
		result = append(result, s[i])
		i++
	}
	return result
}

// HexDecode decodes a hex string to bytes. Returns an error for malformed hex.
func HexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("hex string has odd length %d", len(s))
	}
	result := make([]byte, 0, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		var b byte
		if _, err := fmt.Sscanf(s[i:i+2], "%02x", &b); err != nil {
			return nil, fmt.Errorf("invalid hex at position %d: %w", i, err)
		}
		result = append(result, b)
	}
	return result, nil
}
