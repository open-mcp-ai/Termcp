package transcript

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestRenderRefusesBytesJSONWouldCorrupt pins the refusal that makes offset
// paging trustworthy.
//
// The failure it prevents is silent: jsonResult marshals whatever a read returns,
// and encoding/json REPLACES invalid UTF-8 with U+FFFD instead of reporting it.
// A caller paging by offset then reads from the wrong byte (U+FFFD is 3 bytes
// where the original was 1), and a caller inspecting magic bytes sees mojibake
// where the signature was. Both are unrecoverable after the fact.
func TestRenderRefusesBytesJSONWouldCorrupt(t *testing.T) {
	for name, raw := range map[string][]byte{
		"png magic":  {0x89, 'P', 'N', 'G'},
		"latin-1":    {0xe9, 0xe8},
		"random":     {0xff, 0xfe, 0x80},
		"nul-prefix": {0x00, 0xff},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Render(raw, false)
			if err == nil {
				t.Fatalf("Render accepted % x; a JSON reply would have replaced the invalid bytes with U+FFFD", raw)
			}
			var te *Error
			if !errors.As(err, &te) || te.Kind != ErrInvalid {
				t.Errorf("error = %v, want an ErrInvalid rejection", err)
			}
			// The message has to locate the byte: "not valid UTF-8" alone sends the
			// caller looking through the whole window.
			if !strings.Contains(err.Error(), "byte ") {
				t.Errorf("error = %q, want it to name the offending offset", err)
			}
		})
	}
}

// TestRenderPassesValidBytes is the other half: ordinary output must be
// unaffected, including multi-byte text and control characters JSON can carry.
func TestRenderPassesValidBytes(t *testing.T) {
	for name, raw := range map[string][]byte{
		"ascii":     []byte("hello world\n"),
		"cjk":       []byte("世界 terminal\n"),
		"emoji":     []byte("ok ✅\n"),
		"control":   {0x00, 'A', 0x1f}, // valid UTF-8, all < 0x80
		"empty":     {},
		"utf8 max":  []byte("\U0010FFFF"),
		"tabs/crlf": []byte("a\tb\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := Render(raw, false)
			if err != nil {
				t.Fatalf("Render(% x) refused valid UTF-8: %v", raw, err)
			}
			if out != string(raw) {
				t.Errorf("Render = %q, want %q unchanged", out, string(raw))
			}
		})
	}
}

// TestRenderCheckCatchesRawTheLivePathWouldHide covers the path whose rendered
// string is built elsewhere: the live cursor strips ANSI while reading, so a
// check on its OUTPUT can pass while the stream it came from was binary.
func TestRenderCheckCatchesRawTheLivePathWouldHide(t *testing.T) {
	binary := []byte{0x41, 0xff, 0x42}
	if RemoveANSIStripsToValid(t, binary) {
		t.Fatalf("fixture is not adversarial: the stripped form is already valid")
	}
	if err := RenderCheck(binary); err == nil {
		t.Error("RenderCheck accepted raw bytes the live path would have rendered as mojibake")
	}
	if err := RenderCheck([]byte("plain")); err != nil {
		t.Errorf("RenderCheck refused valid bytes: %v", err)
	}
}

// RemoveANSIStripsToValid reports whether ansi-stripping would launder the bytes
// into something utf8-valid, which is exactly the hole RenderCheck closes.
func RemoveANSIStripsToValid(t *testing.T, raw []byte) bool {
	t.Helper()
	stripped, err := Render(raw, true)
	if err != nil {
		return false
	}
	if stripped == "" {
		return false
	}
	_, err = json.Marshal(stripped)
	return err == nil
}
