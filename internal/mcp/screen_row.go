package mcp

// A SCREEN ROW IS A LINE THAT WAS DISPLAYED, NOT THE BYTES THAT WERE WRITTEN TO IT.
//
// The periodic table of things a terminal applies inside one row is short, and this applies
// exactly that: carriage return, backspace, and the two erases. Everything else — colour,
// cursor motion — is skipped, because the model has already decided which row the bytes
// belong to and colour does not change the characters.
//
// WHY THIS EXISTS (dogfood round two): a shell paints its input line, then repaints it after
// each keystroke with `CSI K` and a carriage return. The row's byte span therefore holds
// every paint in order, and returning it verbatim gave the caller a palimpsest —
//
//	Write-Host -NoNewline "alpha\rbeta\rgamma"; Write-HosWrite-Host -NoNewline "alpha…
//
// which is not what the screen said at any moment. The characters that survived are the last
// paint; applying the row's own control bytes is what recovers them.

import (
	"strings"
	"unicode/utf8"
)

// renderScreenRow returns what one row of raw terminal bytes displays.
func renderScreenRow(raw []byte) string {
	var out []rune
	col := 0
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		switch b {
		case '\r':
			col = 0
			continue
		case '\n':
			// A newline inside a row's span means the span crossed a row boundary; the
			// row ends here rather than carrying the next one's text.
			return trimRow(out)
		case 0x08: // backspace: one column left, nothing erased
			if col > 0 {
				col--
			}
			continue
		case 0x1b:
			// One escape sequence: CSI (`ESC [ … final`), OSC (`ESC ] … BEL|ST`), or a
			// two-byte form.
			n, erase := consumeEscape(raw[i:])
			if erase.kind != 0 {
				switch erase.kind {
				case 'K': // EL: 0 = to end of line, 1 = to start, 2 = whole line
					switch erase.param {
					case 0:
						if col < len(out) {
							out = out[:col]
						}
					case 1:
						for c := 0; c <= col && c < len(out); c++ {
							out[c] = ' '
						}
					case 2:
						// The cursor does not move: `CSI 2K` erases the row, it does not
						// return to column one. Only CR does that.
						for c := range out {
							out[c] = ' '
						}
					}
				case 'J': // ED: a full clear is the only form that matters inside a row
					if erase.param == 2 {
						out = out[:0]
						col = 0
					}
				}
			}
			i += n - 1
			continue
		}
		if b < 0x20 { // other control bytes: dropped, they display nothing
			continue
		}
		// UTF-8: take the whole rune so a multi-byte glyph lands in one cell.
		if b < 0x80 {
			putRune(&out, &col, rune(b))
			continue
		}
		r, size := utf8.DecodeRune(raw[i:])
		if size <= 0 {
			size = 1
		}
		putRune(&out, &col, r)
		i += size - 1
	}
	return trimRow(out)
}

func putRune(out *[]rune, col *int, r rune) {
	for len(*out) < *col {
		*out = append(*out, ' ')
	}
	if *col < len(*out) {
		(*out)[*col] = r
	} else {
		*out = append(*out, r)
	}
	*col++
}

func trimRow(out []rune) string {
	return strings.TrimRight(string(out), " \t")
}

// escapeErase reports the erase an escape sequence performs, if any.
type escapeErase struct {
	kind  byte // 'K' or 'J'
	param int
}

// consumeEscape returns how many bytes the sequence at the start of b occupies and whether
// it erases.
func consumeEscape(b []byte) (int, escapeErase) {
	if len(b) < 2 {
		return len(b), escapeErase{}
	}
	switch b[1] {
	case '[':
		param, i := 0, 2
		seenDigit := false
		for i < len(b) {
			c := b[i]
			if c >= '0' && c <= '9' {
				param = param*10 + int(c-'0')
				seenDigit = true
				i++
				continue
			}
			if c == ';' || c == '?' {
				i++
				continue
			}
			// Final byte of a CSI: letters and a few symbols.
			if c >= 0x40 && c <= 0x7e {
				er := escapeErase{}
				if c == 'K' || c == 'J' {
					if !seenDigit {
						param = 0
					}
					er = escapeErase{kind: c, param: param}
				}
				return i + 1, er
			}
			i++
		}
		return len(b), escapeErase{}
	case ']':
		// OSC ends at BEL or ST (ESC \).
		for i := 2; i < len(b); i++ {
			if b[i] == 0x07 {
				return i + 1, escapeErase{}
			}
			if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
				return i + 2, escapeErase{}
			}
		}
		return len(b), escapeErase{}
	}
	return 2, escapeErase{}
}
