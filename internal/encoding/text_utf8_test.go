package encoding

import "testing"

// TestEncodeTextKeepsValidUTF8 pins the fix for bug #2: mode=text must return
// readable text, not \xHH escapes, for any valid UTF-8 content.
func TestEncodeTextKeepsValidUTF8(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ascii", "hello", "hello"},
		{"chinese", "中文测试", "中文测试"},
		{"mixed", "中文ok", "中文ok"},
		{"emoji", "a🎉b", "a🎉b"},
		{"accents", "café", "café"},
		{"newline stays escaped", "a\nb", `a\nb`},
		{"tab stays escaped", "a\tb", `a\tb`},
		{"control byte escaped", "a\x01b", `a\x01b`},
		{"invalid utf8 escaped", "a\xffb", `a\xffb`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EncodeText([]byte(tc.in)); got != tc.want {
				t.Errorf("EncodeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEncodeDecodeRoundTrip pins that the escape set is still a faithful inverse:
// DecodeText(EncodeText(x)) must return x for arbitrary bytes.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, s := range []string{
		"hello", "中文测试", "中文ok", "a🎉b", "café",
		"a\nb\tc\rd", "a\x01b", "a\xffb", "\x00\x01\xfe\xff",
		"mixed 中文 with \x00 binary \xff bytes",
	} {
		enc := EncodeText([]byte(s))
		got := string(DecodeText(enc))
		if got != s {
			t.Errorf("round trip failed: %q -> %q -> %q", s, enc, got)
		}
	}
}
