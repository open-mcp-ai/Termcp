package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadFile_AutoDetectsUTF16BOM pins #291: Windows tools (a PowerShell `>`
// redirect in particular) write UTF-16 with a BOM. Every ASCII character in that
// encoding has a high NUL byte, so a text read used to refuse the file as binary
// unless the caller guessed to pass `--encoding utf-16`. The BOM is proof, not a
// guess, so a text read with no explicit encoding must decode it.
func TestReadFile_AutoDetectsUTF16BOM(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid := strings.SplitN(h, ":", 2)[0]

	// Names stay free of parentheses: the DSL's parser reads `(` as syntax, and a
	// subtest name lands in the temp path.
	cases := []struct {
		name string
		data []byte
	}{
		{
			name: "UTF16LE-with-BOM-PowerShell-redirect-default",
			data: []byte{0xFF, 0xFE, 'h', 0x00, 'e', 0x00, 'l', 0x00, 'p', 0x00},
		},
		{
			name: "UTF16BE-with-BOM",
			data: []byte{0xFE, 0xFF, 0x00, 'h', 0x00, 'e', 0x00, 'l', 0x00, 'p'},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "out.txt")
			if err := os.WriteFile(p, tc.data, 0o644); err != nil {
				t.Fatal(err)
			}
			posix := "/" + filepath.ToSlash(p)

			got := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + posix})
			if !strings.HasPrefix(got, "TERM/1 200") {
				t.Fatalf("a UTF-16 file with a BOM must read as text, not be refused as binary:\n%s", got)
			}
			// The bytes arrive as the file-read projection, so the decoded text is
			// the value of `data`, not the whole body.
			if !strings.Contains(got, "data: help") {
				t.Fatalf("the decoded text is missing; the BOM was not honoured as proof of the encoding:\n%s", got)
			}
			// The BOM must be consumed, not handed back as U+FEFF or as escapes.
			if strings.Contains(got, "\\ufeff") || strings.Contains(got, "feff") {
				t.Errorf("the BOM was not stripped from the decoded text:\n%s", got)
			}
		})
	}
}

// TestReadFile_BinaryRejectionHintsEncoding pins the message half of #291: an
// un-BOM'd binary file is still refused, but the hint must name `--encoding` — a
// caller with wide text and no BOM had no way to learn that option existed.
func TestReadFile_BinaryRejectionHintsEncoding(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid := strings.SplitN(h, ":", 2)[0]

	dir := t.TempDir()
	p := filepath.Join(dir, "raw.bin")
	if err := os.WriteFile(p, []byte{0x00, 0x01, 0x02, 0x00, 'x'}, 0o644); err != nil {
		t.Fatal(err)
	}
	posix := "/" + filepath.ToSlash(p)

	got := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + posix})
	if !strings.HasPrefix(got, "TERM/1 400") {
		t.Fatalf("a real binary file must still be refused:\n%s", got)
	}
	if !strings.Contains(got, "--encoding utf-16") {
		t.Errorf("the refusal must name `--encoding utf-16` as the wide-charset remedy:\n%s", got)
	}
}

// TestReadFile_ExplicitEncodingSilencesTheBinaryCheck confirms an explicit
// `--encoding` still overrules auto-detection, including for wide text with no BOM.
func TestReadFile_ExplicitEncodingSilencesTheBinaryCheck(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid := strings.SplitN(h, ":", 2)[0]

	dir := t.TempDir()
	p := filepath.Join(dir, "wide.txt")
	// No BOM: 'a', 'b' with high NUL bytes.
	if err := os.WriteFile(p, []byte{'a', 0x00, 'b', 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	posix := "/" + filepath.ToSlash(p)

	got := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + posix + " --encoding utf-16le"})
	if !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("an explicit --encoding must silence the binary check:\n%s", got)
	}
	if !strings.Contains(got, "data: ab") {
		t.Fatalf("expected the decoded text 'ab':\n%s", got)
	}
}
