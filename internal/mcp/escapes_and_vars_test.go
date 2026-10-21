package mcp

import (
	"strings"
	"testing"
)

// TestRawControlBytesRefusedByDefault pins the escape gate: unquoted control bytes
// in cmd are refused with a 400 unless allow_escapes is explicitly true.
//
// The bytes checked are the ones a terminal acts on:
//   - CR (0x0d): submits the line, executing two commands for one call
//   - Ctrl-C (0x03): interrupts a command
//   - Ctrl-D (0x04): sends EOF, exiting an interactive shell
//   - Ctrl-Z (0x1a): suspends a command
//   - NUL (0x00): truncates / abnormal
func TestRawControlBytesRefusedByDefault(t *testing.T) {
	s := newTestServer(t)

	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"bare CR", "run a:1 echo a\rb", "carriage return"},
		{"bare Ctrl-C", "run a:1 echo a\x03b", "Ctrl-C"},
		{"bare Ctrl-D", "run a:1 echo a\x04b", "Ctrl-D"},
		{"bare Ctrl-Z", "run a:1 echo a\x1ab", "Ctrl-Z"},
		{"bare NUL", "run a:1 echo a\x00b", "NUL"},
		{"bare ESC", "run a:1 echo a\x1bb", "ESC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := termText(t, s, map[string]any{"cmd": tc.cmd})
			if !strings.HasPrefix(got, "TERM/1 4") {
				t.Fatalf("unquoted %s must be refused with 400:\n%s", tc.name, got)
			}
			if !strings.Contains(got, "raw_control_byte") {
				t.Errorf("error must have Error-Code: raw_control_byte:\n%s", got)
			}
			if !strings.Contains(got, "allow_escapes") || !strings.Contains(got, "vars") {
				t.Errorf("error must teach both allow_escapes and env as the way out:\n%s", got)
			}
		})
	}
}

// TestAllowEscapesTruePermitsControlBytes verifies that the explicit switch works.
func TestAllowEscapesTruePermitsControlBytes(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// With allow_escapes: true, the raw control byte is permitted through to the shell.
	got := termText(t, s, map[string]any{
		"cmd":           "run " + h + " echo a\rb",
		"allow_escapes": true,
	})
	if strings.HasPrefix(got, "TERM/1 4") && strings.Contains(got, "raw_control_byte") {
		t.Fatalf("allow_escapes: true must permit control bytes:\n%s", got)
	}
}

// TestQuotedControlBytesArePermittedByDefault: single-quoted or double-quoted control
// bytes are data, not unquoted syntax, so they are not rejected.
func TestQuotedControlBytesArePermittedByDefault(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for _, cmd := range []string{
		"run " + h + " echo 'a\rb'",
		"run " + h + ` echo "a\rb"`,
		"run " + h + " echo a\tb", // TAB is normal whitespace, not a dangerous control byte
	} {
		got := termText(t, s, map[string]any{"cmd": cmd})
		if strings.Contains(got, "raw_control_byte") {
			t.Errorf("quoted/safe control byte was wrongly rejected:\n%s", got)
		}
	}
}

// TestVarsASTSubstitution verifies that `vars` replaces variable references at parse
// time, safely quoted so spaces/quotes/newlines never become shell syntax.
func TestVarsASTSubstitution(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	cases := []struct {
		name string
		cmd  string
		env  map[string]any
		want string // string that must be printed in the output
	}{
		{
			"simple substitution",
			"run " + h + " echo $MYVAR",
			map[string]any{"MYVAR": "hello_from_env"},
			"hello_from_env",
		},
		{
			"braced variable",
			"run " + h + " echo ${MYVAR}",
			map[string]any{"MYVAR": "braced_works"},
			"braced_works",
		},
		{
			"value with spaces stays one word",
			"run " + h + " printf '[%s]\\n' $VAL",
			map[string]any{"VAL": "a b c"},
			"[a b c]",
		},
		{
			"value with semicolon does not split statement",
			"run " + h + " echo $PAYLOAD",
			map[string]any{"PAYLOAD": "echo A; echo B"},
			"echo A; echo B",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := termText(t, s, map[string]any{
				"cmd": tc.cmd,
				"vars": tc.env,
			})
			if strings.HasPrefix(got, "TERM/1 4") {
				t.Fatalf("call failed with 4xx:\n%s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("output does not contain %q:\n%s", tc.want, got)
			}
		})
	}
}

// TestVarsMissingVariableIsRefused pins that referencing an undefined variable in
// `cmd` when `vars` was passed is a 400, not a silent pass-through to whatever the
// remote shell happens to have in its environment.
func TestVarsMissingVariableIsRefused(t *testing.T) {
	s := newTestServer(t)

	got := termText(t, s, map[string]any{
		"cmd": "run a:1 echo $UNDEFINED_VAR",
		"vars": map[string]any{"OTHER": "x"},
	})
	if !strings.HasPrefix(got, "TERM/1 4") {
		t.Fatalf("missing vars variable must be refused with 400:\n%s", got)
	}
	if !strings.Contains(got, "UNDEFINED_VAR") {
		t.Errorf("error must name the missing variable:\n%s", got)
	}
}

// TestVarsArgumentTypeValidation pins that `vars` must be an object of strings.
func TestVarsArgumentTypeValidation(t *testing.T) {
	s := newTestServer(t)

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"vars is array", map[string]any{"cmd": "ps", "vars": []any{"a"}}, "vars must be an object"},
		{"vars is string", map[string]any{"cmd": "ps", "vars": "FOO=1"}, "vars must be an object"},
		{"vars value is number", map[string]any{"cmd": "ps", "vars": map[string]any{"FOO": 42}}, "vars[FOO] must be a string"},
		{"vars key is empty", map[string]any{"cmd": "ps", "vars": map[string]any{"": "val"}}, "empty name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Term(t.Context(), "", tc.args).Text()
			if !strings.HasPrefix(got, "TERM/1 4") {
				t.Fatalf("malformed vars must be 400:\n%s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("error must contain %q:\n%s", tc.want, got)
			}
		})
	}
}
