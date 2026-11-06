package directive

import (
	"strings"
	"testing"
)

// TestTheValueIsInsertedAsWritten pins the one insertion rule left: termcp does not
// touch the caller's bytes.
//
// There used to be a quoting rule here — a value in a shell line became one
// single-quoted word — and it was the wrong layer. Quoting a value only makes sense if
// the value is DATA for a shell line, but a variable in a shell line is not an operand:
// it IS the statement, filling a hole the DSL left open. Wrapping it in quotes would
// rewrite the caller's program.
//
// So the property is now subtraction: whatever the caller supplied arrives, byte for
// byte, in every position that is legal.
func TestTheValueIsInsertedAsWritten(t *testing.T) {
	const value = "cat <<EOF\nit's a $x; test\nEOF\n"
	cases := []struct {
		name string
		cmd  string
		sub  Subst
		read func(Statement) string
	}{
		{"whole payload", `run h:1 $P`,
			Subst{Named: map[string]string{"P": value}},
			func(st Statement) string { return st.Payload }},
		{"braced whole payload", `run h:1 ${P}`,
			Subst{Named: map[string]string{"P": value}},
			func(st Statement) string { return st.Payload }},
		{"positional whole payload", `run h:1 $1`,
			Subst{Pos: []string{value}},
			func(st Statement) string { return st.Payload }},
		{"flag value", `write h file /tmp/x --data $P`,
			Subst{Named: map[string]string{"P": value}},
			func(st Statement) string { v, _ := st.Flag("data"); return v }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := ParseWithSubst(c.cmd, c.sub)
			if err != nil {
				t.Fatalf("%s: %v", c.cmd, err)
			}
			want := value
			if got := c.read(f.Statements[0]); got != want {
				t.Errorf("%s: inserted %q, want %q (nothing rewrites the value)", c.name, got, want)
			}
		})
	}

	// The complement: there is no position where a value is quoted FOR the caller, so a
	// caller who wants one word writes the quotes inside the value.
	f, err := ParseWithSubst(`run h:1 $P`, Subst{Named: map[string]string{"P": `echo 'a b'`}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.Statements[0].Payload, `echo 'a b'`; got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}

	// An unused vars key IS an error now, and the reversal is the point: a table that is
	// wrong by one entry used to be invisible, because the value was simply not applied and
	// the reply looked the same as a correct one.
	if _, err := ParseWithSubst(`run h:1 echo plain`, Subst{Named: map[string]string{"UNUSED": "x"}}); err == nil {
		t.Fatal("an unused vars key was accepted")
	} else if !strings.Contains(err.Error(), "$UNUSED") {
		t.Errorf("the refusal does not name the unused key: %v", err)
	}
}

// TestParseWithSubstUndefinedVariableIsAnError: a variable the caller used but env
// does not define is refused, and the message names it. Leaving it for the shell to
// expand would let cmd and env disagree silently.
func TestParseWithSubstUndefinedVariableIsAnError(t *testing.T) {
	_, err := ParseWithSubst("run h:1 echo $NOPE", Subst{Named: map[string]string{"OTHER": "x"}})
	if err == nil {
		t.Fatal("an undefined variable must be an error when vars is in use")
	}
	if derr, ok := err.(*Error); !ok || derr.Msg == "" {
		t.Fatalf("want a *directive.Error with a message, got %T: %v", err, err)
	}
}

// TestParseWithoutSubstLeavesVariablesAlone: the shell expansion path is unchanged
// when no vars is supplied. A caller that wants $HOME resolved remotely still gets
// that, which is why substitution is opt-in per call.
func TestParseWithoutSubstLeavesVariablesAlone(t *testing.T) {
	// A payload that IS a reference is a payload that is a reference, whether or not a
	// table was supplied: the shape is what the directive declares, and `Parse` has no
	// table to fill it from.
	f, err := Parse("run h:1 $HOME")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Payload; got != "$HOME" {
		t.Errorf("payload = %q, want %q", got, "$HOME")
	}

	// The shell's own `$HOME` is now the caller's to write inside a VALUE, where nothing
	// rescans it: `run h:1 $1` with args ["echo $HOME"] types exactly that.
	f, err = ParseWithSubst("run h:1 $1", Subst{Pos: []string{"echo $HOME"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Payload; got != "echo $HOME" {
		t.Errorf("payload = %q, want %q", got, "echo $HOME")
	}
}

// TestDetectRawControl covers the gate's classification: every raw control byte is a
// terminal action, TAB and newline are ordinary whitespace, and QUOTING CHANGES
// NOTHING.
//
// The last part is the correction. The gate used to skip quoted sections on the
// theory that a quoted byte is the caller's data — true for the shell, false for the
// terminal, which parses no quotes and acts on the byte regardless. Measured with a
// real pty: a carriage return inside single quotes submitted the line early, left
// PowerShell at its `>>` continuation prompt, and the reply was 200.
func TestDetectRawControl(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		found bool
	}{
		{"bare CR", "run h:1 echo a\rb", true},
		{"bare Ctrl-C", "run h:1 echo a\x03b", true},
		{"bare Ctrl-D", "run h:1 echo a\x04b", true},
		{"bare Ctrl-Z", "run h:1 echo a\x1ab", true},
		{"bare NUL", "run h:1 echo a\x00b", true},
		{"bare ESC", "run h:1 echo a\x1bb", true},
		{"single-quoted CR is still an action", "run h:1 echo 'a\rb'", true},
		{"double-quoted CR is still an action", "run h:1 echo \"a\rb\"", true},
		{"escaped-looking CR is still an action", "run h:1 echo a\\\rb", true},
		{"TAB is ordinary whitespace", "run h:1 echo a\tb", false},
		{"newline separates statements, not a gate concern", "run h:1 echo a\nrun h:1 echo b", false},
		{"plain command", "run h:1 ls -la", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, name, found := DetectRawControl(tc.src)
			if found != tc.found {
				t.Fatalf("DetectRawControl(%q) found=%v, want %v (%s)", tc.src, found, tc.found, name)
			}
			if found && name == "" {
				t.Error("a detected byte must be named, so the error can teach")
			}
		})
	}
}
