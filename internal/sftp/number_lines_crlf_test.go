package sftp

import (
	"strings"
	"testing"
)

// TestNumberLinesStripsCRLF pins issue #208.6.
//
// A Windows-authored file contains CRLF endings; numbering the raw slices used to
// append "\r" before "\n" on each numbered row. The "\r" is invisible in a terminal
// and corrupts any downstream line editing.
func TestNumberLinesStripsCRLF(t *testing.T) {
	crlf := "first\r\nsecond\r\nthird\r\n"
	got := numberLines(crlf, 1)
	if strings.Contains(got, "\r") {
		t.Errorf("numberLines left a carriage return in the output:\n%q", got)
	}
	want := "1\tfirst\n2\tsecond\n3\tthird\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A trailing line with no newline is handled identically.
func TestNumberLinesStripsCRLFWithoutTrailingNewline(t *testing.T) {
	crlf := "first\r\nsecond\r"
	got := numberLines(crlf, 1)
	if strings.Contains(got, "\r") {
		t.Errorf("numberLines left a carriage return on the unterminated line:\n%q", got)
	}
	want := "1\tfirst\n2\tsecond"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
