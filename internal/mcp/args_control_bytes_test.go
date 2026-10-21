package mcp

import (
	"runtime"
	"strings"
	"testing"
)

// TestControlBytesInValuesAreRefused pins the gate that cmd-only scanning missed.
//
// A value reaches the terminal without appearing in cmd, so scanning cmd alone let
// `args: ["a\x03b"]` through. What the terminal then did was worse than a missing
// check: it acted on the byte, echoed `^C`, and the shell dropped into its `quote>`
// continuation prompt — while the reply still said 200.
//
// Quoting is no defence. A control byte inside single quotes is still a control byte
// to the terminal, which is exactly why the gate has to look at the VALUE.
func TestControlBytesInValuesAreRefused(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	cases := []struct {
		name  string
		args  map[string]any
		where string
	}{
		{
			"ctrl-c in args",
			map[string]any{"cmd": "run " + h + " echo $1", "args": []any{"a\x03b"}},
			"args[0]",
		},
		{
			"carriage return in args",
			map[string]any{"cmd": "run " + h + " echo $1", "args": []any{"a\rb"}},
			"args[0]",
		},
		{
			"ctrl-c in vars",
			map[string]any{"cmd": "run " + h + " echo $V", "vars": map[string]any{"V": "a\x03b"}},
			"vars[V]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := termText(t, s, tc.args)
			if !strings.HasPrefix(got, "TERM/1 4") {
				t.Fatalf("a control byte must be refused, got:\n%s", got)
			}
			if !strings.Contains(got, tc.where) {
				t.Errorf("the refusal should name where the byte is (%q):\n%s", tc.where, got)
			}
		})
	}
}

// TestControlBytesPassWhenAllowed pins that allow_escapes is still the way out: the
// gate reports, it does not decide for the caller who meant the byte.
func TestControlBytesPassWhenAllowed(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd": "run " + h + " echo $1", "args": []any{"x\x03y"}, "allow_escapes": true,
	})

	// ON WINDOWS THE BYTE ARRIVES AND THEN DOES ITS JOB. Writing 0x03 into a console
	// raises Ctrl-C there, so the shell dies with STATUS_CONTROL_C_EXIT before it can
	// echo anything. That is the byte being delivered, not the gate refusing it -- the
	// gate's refusal is a 400 with a message, which is what the case above asserts.
	// Requiring a 200 here would mean requiring the platform to ignore the byte.
	if strings.HasPrefix(got, "TERM/1 4") {
		t.Fatalf("allow_escapes must not be refused by the gate:\n%s", got)
	}
	if runtime.GOOS == "windows" {
		if !strings.Contains(got, "3221225786") {
			t.Logf("windows did not report STATUS_CONTROL_C_EXIT; the byte may have been "+
				"buffered instead of delivered:\n%s", got)
		}
		return
	}
	if !strings.HasPrefix(got, "TERM/1 2") {
		t.Fatalf("allow_escapes must let the byte through:\n%s", got)
	}
}
