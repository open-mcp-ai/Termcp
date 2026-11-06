package directive

import "testing"

// TestAPayloadBeforeASeparatorIsStillTheCommandLine pins issues #141 and #155, and the
// rule that replaced them.
//
// The span used to be a BYTE RANGE, and one trailing space was enough to answer "no" to
// "is this positional the payload?": `run h:1 $1 &` sent the terminal `'echo C' &` — the
// value arrived, and the command had silently stopped being a command line. The fix was
// to take the payload's bounds from the AST word list, and the cases below are what that
// fix had to keep working.
//
// THE PAYLOAD IS NOW EXACTLY ONE REFERENCE, which changes what "the line" means: the
// shell text is the VALUE, so there is nothing beside the reference for a `&` or a `|`
// to belong to. The punctuation cases below therefore split in two — the ones that are
// this language's own (`&`, `;`, `&&`, `||`) still end or sequence the statement, and the
// ones that are the shell's (`|`, `>`) are refused, because with a literal payload gone
// there is no way left to tell which language wrote them.
func TestAPayloadBeforeASeparatorIsStillTheCommandLine(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		args []string
		// want is the payload of each statement, in order.
		want []string
	}{
		{"background", `run h:1 $1 &`, []string{"echo C"}, []string{"echo C &"}},
		{"separator", `run h:1 $1 ; ls host`, []string{"echo D"}, []string{"echo D ", ""}},
		{"and chain", `run h:1 $1 && run h:1 $2`, []string{"echo A", "echo B"}, []string{"echo A", "echo B"}},
		{"or chain", `run h:1 $1 || run h:1 $2`, []string{"echo A", "echo B"}, []string{"echo A", "echo B"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseWithSubst(tc.cmd, Subst{Pos: tc.args})
			if err != nil {
				t.Fatalf("ParseWithSubst(%q): %v", tc.cmd, err)
			}
			if len(f.Statements) != len(tc.want) {
				t.Fatalf("%q parsed into %d statements, want %d", tc.cmd, len(f.Statements), len(tc.want))
			}
			for i, want := range tc.want {
				if got := f.Statements[i].Payload; got != want {
					t.Errorf("%q statement %d payload = %q, want %q", tc.cmd, i, got, want)
				}
			}
		})
	}

	// THE SHELL'S PUNCTUATION IS REFUSED, and it is refused for the reason the whole
	// rule exists: `$1 | wc -l` is a pipeline to one reader and a termcp pipe to the
	// other. The pipeline belongs in the value.
	for _, cmd := range []string{`run h:1 $1 | wc -l`, `run h:1 $1 > /tmp/out`} {
		t.Run(cmd, func(t *testing.T) {
			if _, err := ParseWithSubst(cmd, Subst{Pos: []string{"echo C"}}); err == nil {
				t.Fatalf("%q was accepted; the shell's punctuation has no payload to belong to", cmd)
			}
		})
	}

	// A reference with a word BEFORE it was already refused, and still is.
	t.Run("word before it is refused", func(t *testing.T) {
		if _, err := ParseWithSubst(`run h:1 echo $1`, Subst{Pos: []string{"a; b"}}); err == nil {
			t.Fatal("a reference beside shell text was accepted; DSL and shell are not mixed")
		}
	})
}
