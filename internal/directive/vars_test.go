package directive

import "testing"

// TestParseWithSubstQuotesValuesByContext pins the quoting rule: a value substituted
// into an unquoted word becomes a single-quoted shell word, and one substituted
// inside an existing double-quoted string is escaped for that context.
//
// This is the property the whole `vars` channel rests on. A value carrying a space,
// a quote, a semicolon or a newline must reach the shell as ONE word of DATA — if
// the quoting were wrong, the value would become shell SYNTAX and the caller would
// be running a command they never wrote.
func TestParseWithSubstQuotesValuesByContext(t *testing.T) {
	cases := []struct {
		name     string
		cmd      string
		vars     map[string]string
		wantPart string // must appear in the resulting Payload
	}{
		{
			name:     "unquoted word gets single quotes",
			cmd:      "run h:1 echo $P",
			vars:     map[string]string{"P": "A; B"},
			wantPart: "echo 'A; B'",
		},
		{
			name:     "single quote inside the value is escaped",
			cmd:      "run h:1 echo $P",
			vars:     map[string]string{"P": "it's"},
			wantPart: `echo 'it'\''s'`,
		},
		{
			name:     "inside double quotes the value is escaped, not re-quoted",
			cmd:      `run h:1 echo "$P"`,
			vars:     map[string]string{"P": "a b"},
			wantPart: `echo "a b"`,
		},
		{
			name:     "a dollar in the value is escaped inside double quotes",
			cmd:      `run h:1 echo "$P"`,
			vars:     map[string]string{"P": "$HOME"},
			wantPart: `echo "\$HOME"`,
		},
		{
			name:     "braced form is replaced too",
			cmd:      "run h:1 echo ${P}",
			vars:     map[string]string{"P": "braced"},
			wantPart: "echo 'braced'",
		},
		{
			name:     "both forms in one payload",
			cmd:      "run h:1 echo $A ${B}",
			vars:     map[string]string{"A": "one", "B": "two"},
			wantPart: "echo 'one' 'two'",
		},
		{
			name:     "an unused vars key is not an error",
			cmd:      "run h:1 echo plain",
			vars:     map[string]string{"UNUSED": "x"},
			wantPart: "echo plain",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseWithSubst(tc.cmd, Subst{Named: tc.vars})
			if err != nil {
				t.Fatalf("ParseWithVars(%q): %v", tc.cmd, err)
			}
			if len(f.Statements) != 1 {
				t.Fatalf("want 1 statement, got %d", len(f.Statements))
			}
			if got := f.Statements[0].Payload; got != tc.wantPart {
				t.Errorf("payload = %q, want %q", got, tc.wantPart)
			}
		})
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
	f, err := Parse("run h:1 echo $HOME")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Payload; got != "echo $HOME" {
		t.Errorf("payload = %q, want %q", got, "echo $HOME")
	}
}

// TestDetectRawControl covers the gate's classification: unquoted control bytes are
// terminal actions, quoted ones are the caller's data, and TAB/newline are ordinary
// whitespace.
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
		{"single-quoted CR is data", "run h:1 echo 'a\rb'", false},
		{"double-quoted CR is data", "run h:1 echo \"a\rb\"", false},
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
