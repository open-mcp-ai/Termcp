package directive

import (
	"strings"
	"testing"
)

// TestPayloadFlagsAtEndRefused pins #285:
// A flag written at the END of a payload verb (e.g. `call h {"a":1} --json` or
// `call h cdp:1 {"method":"x"} --send`) used to be swallowed into the payload raw.
// For frames, the JSON parser then reported "invalid character '-' after top-level value",
// pointing the caller at a JSON document that was completely valid.
//
// Flags must come BEFORE the payload, not after it.
func TestPayloadFlagsAtEndRefused(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		flag string
	}{
		{
			name: "call trailing boolean flag",
			cmd:  `call h {"method":"Page.enable"} --send`,
			flag: "--send",
		},
		{
			name: "call trailing json flag",
			cmd:  `call h cdp:1 {"method":"Page.enable"} --json`,
			flag: "--json",
		},
		{
			name: "call trailing flag with equals",
			cmd:  `call h {"a":1} --json=true`,
			flag: "--json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.cmd)
			if err == nil {
				t.Fatalf("expected parse error for %s, got nil", tc.cmd)
			}
			msg := err.Error()
			if !strings.Contains(msg, "flags come before the payload") {
				t.Errorf("error message missing 'flags come before the payload': %s", msg)
			}
			if !strings.Contains(msg, tc.flag) {
				t.Errorf("error message missing flag name %q: %s", tc.flag, msg)
			}
		})
	}
}

// TestPayloadTailFlagOnlyRefusesDeclaredFlags confirms that only a DECLARED flag
// at the tail is refused: an unknown flag or a string inside quotes is not a flag
// and must remain untouched.
func TestPayloadTailFlagOnlyRefusesDeclaredFlags(t *testing.T) {
	ok := []string{
		`run s1:0 $1`,
		`call h {"a":1} --bogus`,
		`call h '{"cmd":"--json"}'`,
		`call h {"a":1}`,
	}
	for _, cmd := range ok {
		if _, err := Parse(cmd); err != nil {
			t.Errorf("Parse(%q) must stay legal: %v", cmd, err)
		}
	}
}
