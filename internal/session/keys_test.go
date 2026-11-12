package session

import "testing"

// `help key` HAS ALWAYS LISTED THESE THREE and the parser refused them: a TUI with no line
// editor (vim, less, htop) has no other way to receive a space or a page, and the refusal
// listed a set that contradicted the help page it pointed at.
func TestKeyBytesSendsSpaceAndPages(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want []byte
	}{
		{"space", []byte{' '}},
		{"pageup", []byte{0x1b, '[', '5', '~'}},
		{"pagedown", []byte{0x1b, '[', '6', '~'}},
		// Case and padding are the caller's, not the vocabulary's.
		{" Space ", []byte{' '}},
		{"PAGEUP", []byte{0x1b, '[', '5', '~'}},
	} {
		got, err := KeyBytes(tc.key, true, false)
		if err != nil {
			t.Fatalf("KeyBytes(%q): %v", tc.key, err)
		}
		if string(got) != string(tc.want) {
			t.Fatalf("KeyBytes(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}
