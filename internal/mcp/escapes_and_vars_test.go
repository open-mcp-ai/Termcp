package mcp

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRawControlBytesAreRefusedOnlyInAShellLine pins where the gate sits now.
//
// It used to scan cmd as typed and refuse every control byte wherever it was
// headed, with `allow_escapes` as the override. That was the wrong question: cmd
// is DSL text and a shell line is a VALUE, so the byte's danger comes from the
// LAYER that reads it, not from the request it arrived in. A path may contain a
// carriage return; a terminal acts on one.
//
// So the check reads parsed statements, and only the regions that reach a shell —
// the `--argv` word list, and the payload a value is substituted into. A run/input
// payload is ONE VARIABLE REFERENCE, so the shell line itself always travels as a
// value: every case below writes the byte into args and asserts the same refusal.
// The override is gone with the question: a keystroke is a directive (`key
// <channel> ctrl-c`), and there is no legitimate control byte in a shell line to
// make an exception for.
func TestRawControlBytesAreRefusedOnlyInAShellLine(t *testing.T) {
	s := newTestServer(t)

	// The bytes a terminal acts on, each one refused once it lands in the payload's
	// value. The command is the same in every case; only the shell line changes.
	const cmd = "run a:1 $1"
	cases := []struct {
		name string
		sh   string
		want string
	}{
		{"bare CR", "echo a\rb", "carriage return"},
		{"bare Ctrl-C", "echo a\x03b", "Ctrl-C"},
		{"bare Ctrl-D", "echo a\x04b", "Ctrl-D"},
		{"bare Ctrl-Z", "echo a\x1ab", "Ctrl-Z"},
		{"bare NUL", "echo a\x00b", "NUL"},
		{"bare ESC", "echo a\x1bb", "ESC"},
		// Quoting never made a byte safe: the terminal reads the line before any
		// shell parses quotes, so a quoted control byte is exactly as dangerous.
		{"quoted CR", "echo 'a\rb'", "carriage return"},
		{"double-quoted CR", "echo \"a\rb\"", "carriage return"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := termText(t, s, map[string]any{"cmd": cmd, "args": []any{tc.sh}})
			if !strings.HasPrefix(got, "TERM/1 4") {
				t.Fatalf("a control byte in a shell line must be refused with 400:\n%s", got)
			}
			if !strings.Contains(got, "raw_control_byte") {
				t.Errorf("error must have Error-Code: raw_control_byte:\n%s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("the refusal should name the byte (%q):\n%s", tc.want, got)
			}
			// The teaching is the point: a keystroke is a directive, and data goes
			// somewhere no terminal reads.
			if !strings.Contains(got, "key <channel> ctrl-c") || !strings.Contains(got, "--data") {
				t.Errorf("the refusal must teach both ways out:\n%s", got)
			}
		})
	}

	// TAB is whitespace to every layer and must stay ordinary.
	got := termText(t, s, map[string]any{"cmd": cmd, "args": []any{"echo a\tb"}})
	if strings.Contains(got, "raw_control_byte") {
		t.Errorf("TAB is ordinary whitespace and must not be refused:\n%s", got)
	}

	// AND THE DATA CASE PASSES. A carriage return in a path names a file, and no
	// terminal ever sees it — the caller's bytes are the answer. This used to need
	// `allow_escapes: true` to get through, which is what made the switch a lie: the
	// byte was never going to reach a line editor.
	rec := postTerm(t, s, "{\"cmd\": \"get \\\"a\\rb\\\"\"}")
	slots := termSlots(t, rec)
	// The session does not exist (404), and that is the point: the byte reached the
	// handler as DATA. What must not happen is the gate answering first.
	if code, _ := slots[0]["Error-Code"].(string); code == "raw_control_byte" {
		t.Fatalf("a CR in a path is data and must not be refused: %v", slots[0])
	}
	path, _ := slots[0]["Path"].(string)
	if !strings.Contains(path, "\r") {
		t.Errorf("the path lost its CR on the way back: %q", path)
	}
}

// TestAKeystrokeIsADirectiveNotAByte pins the replacement for `allow_escapes`.
//
// The switch existed so a caller could send Ctrl-C. It should never have been a
// byte in a command line: `key <channel> ctrl+c` says what it means, and the
// terminal gets the byte without any line having to carry it.
func TestAKeystrokeIsADirectiveNotAByte(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// Prime the shell so the interrupt below lands on a running line editor.
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo READY"}, "timeout": 2})
	waitForGrepContent(t, s, h, "READY")

	// `ctrl+c`, the spelling the directive publishes. The test used `ctrl-c`, which the
	// key table does not answer, so on every platform that runs the assertion below it was
	// checking a 400 for an unknown key and reporting "the gate refused the byte".
	got := termText(t, s, map[string]any{"cmd": "key " + h + " ctrl+c"})
	if strings.Contains(got, "raw_control_byte") {
		t.Fatalf("the key directive must not be refused by the gate:\n%s", got)
	}
	// ON WINDOWS THE BYTE ARRIVES AND THEN DOES ITS JOB: writing 0x03 into a console
	// raises Ctrl-C there, so the shell dies with STATUS_CONTROL_C_EXIT. That is the
	// byte being delivered, not the gate refusing it.
	if runtime.GOOS == "windows" {
		return
	}
	if !strings.HasPrefix(got, "TERM/1 2") {
		t.Fatalf("key must let the byte through:\n%s", got)
	}
}

// TestControlBytesInValuesReachTheRegionTheyLandIn pins that a value cannot
// smuggle a byte past the gate.
//
// A value reaches the terminal through `args`/`vars` without appearing in cmd at
// all, so scanning cmd alone let `args: ["a\x03b"]` through — and what the terminal
// then did was worse than a missing check: it acted on the byte, echoed `^C`, and
// the shell dropped into its continuation prompt while the reply still said 200.
//
// The gate reads the PARSED payload, so a value substituted into one is in the
// region it is checked in. The same bytes in a `--data` flag go to a destination no
// terminal reads and pass.
func TestControlBytesInValuesReachTheRegionTheyLandIn(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"ctrl-c in args", map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo a\x03b"}}},
		{"carriage return in args", map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo a\rb"}}},
		{"ctrl-c in vars", map[string]any{"cmd": "run " + h + " $V", "vars": map[string]any{"V": "echo a\x03b"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := termText(t, s, tc.args)
			if !strings.HasPrefix(got, "TERM/1 4") {
				t.Fatalf("a control byte substituted into a payload must be refused:\n%s", got)
			}
			if !strings.Contains(got, "raw_control_byte") {
				t.Errorf("error must have Error-Code: raw_control_byte:\n%s", got)
			}
		})
	}

	// The complement, end to end: the same byte in --data is data.
	got := termText(t, s, map[string]any{
		"cmd":  "write " + h + " file /tmp/ctlb.txt --data $V",
		"vars": map[string]any{"V": "line1\rline2"},
	})
	if strings.Contains(got, "raw_control_byte") {
		t.Errorf("a control byte in --data goes nowhere near a terminal:\n%s", got)
	}
}

// TestVarsASTSubstitution verifies that `vars` fills the hole in cmd with the
// caller's shell line, byte for byte, and that the terminal runs it.
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
			// THE SHELL LINE IS THE VALUE. The reference is the whole payload, so what
			// arrives is the statement the caller put in `vars`.
			"simple substitution",
			"run " + h + " --force $MYVAR",
			map[string]any{"MYVAR": "echo hello_from_env"},
			"hello_from_env",
		},
		{
			"braced variable",
			"run " + h + " --force ${MYVAR}",
			map[string]any{"MYVAR": "echo braced_works"},
			"braced_works",
		},
		{
			"value with spaces arrives as written",
			"run " + h + " $VAL",
			map[string]any{"VAL": "printf '[%s]\\n' 'a b c'"},
			"[a b c]",
		},
		{
			// A semicolon in the value IS a statement separator, because the value is a
			// statement. Both halves run; the caller's text was not data to be quoted.
			"a semicolon in the value separates statements",
			"run " + h + " --force $PAYLOAD",
			map[string]any{"PAYLOAD": "echo A; echo B"},
			"echo B",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// POLL, DO NOT RACE THE SHELL. One `run` returns what has arrived by the
			// time the window closes, which under -race on a loaded runner can be the
			// echoed command line alone — the output was coming, the caller just asked
			// too early. Every attempt is a fresh `run`, so a late first read still
			// sees the result once the shell has produced it.
			var got string
			for i := 0; i < 40; i++ {
				got = termText(t, s, map[string]any{
					"cmd":  tc.cmd,
					"vars": tc.env,
				})
				if strings.HasPrefix(got, "TERM/1 4") {
					t.Fatalf("call failed with 4xx:\n%s", got)
				}
				if strings.Contains(got, tc.want) {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Errorf("output does not contain %q:\n%s", tc.want, got)
		})
	}
}
