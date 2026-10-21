package directive

import (
	"strings"
	"testing"
)

// TestArgsIsTheSyntaxChannel pins the rule the args channel exists for: a positional
// value that IS the whole payload reaches the terminal as a command line, not as one
// quoted word.
//
// The case that forced it: `run h:1 $1` with args=["pwd; whoami"]. Quoted, the shell
// receives `'pwd; whoami'` and answers "command not found: pwd; whoami" — the caller
// asked for two commands and got a search for a program with a semicolon in its name.
func TestArgsIsTheSyntaxChannel(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		sub  Subst
		want []string
	}{
		{
			name: "a whole payload is spliced verbatim",
			cmd:  `run h:1 $1`,
			sub:  Subst{Pos: []string{"pwd; whoami"}},
			want: []string{"pwd; whoami"},
		},
		{
			name: "a loop reaches the shell as a loop",
			cmd:  `run h:1 $1`,
			sub:  Subst{Pos: []string{"for i in 1 2; do echo $i; done"}},
			want: []string{"for i in 1 2; do echo $i; done"},
		},
		{
			name: "each statement takes its own argument",
			cmd:  `run h:1 $1; run h:1 $2`,
			sub:  Subst{Pos: []string{"a; b", "c | d"}},
			want: []string{"a; b", "c | d"},
		},
		{
			name: "a newline in the value survives",
			cmd:  `run h:1 $1`,
			sub:  Subst{Pos: []string{"line1\nline2"}},
			want: []string{"line1\nline2"},
		},
		{
			name: "a value that looks like a directive is still one payload",
			cmd:  `run h:1 $1`,
			sub:  Subst{Pos: []string{"ps; help"}},
			want: []string{"ps; help"},
		},
		{
			name: "type takes a raw payload too",
			cmd:  `type h:1 $1`,
			sub:  Subst{Pos: []string{"a; b"}},
			want: []string{"a; b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseWithSubst(tc.cmd, tc.sub)
			if err != nil {
				t.Fatalf("ParseWithSubst(%q): %v", tc.cmd, err)
			}
			if len(f.Statements) != len(tc.want) {
				t.Fatalf("statements = %d, want %d", len(f.Statements), len(tc.want))
			}
			for i, want := range tc.want {
				if got := f.Statements[i].Payload; got != want {
					t.Errorf("statement %d payload = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// TestNamedValueIsTheDataChannel pins the other side of the split: a `vars` value is
// one shell WORD, so a newline or a semicolon in it is data rather than syntax.
//
// The two channels have opposite quoting because they carry opposite things. A
// payload is a command line; a flag value is a value.
func TestNamedValueIsTheDataChannel(t *testing.T) {
	f, err := ParseWithSubst(`run h:1 $CMD`, Subst{Named: map[string]string{"CMD": "pwd; whoami"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.Statements[0].Payload, "'pwd; whoami'"; got != want {
		t.Errorf("payload = %q, want %q (one quoted word)", got, want)
	}

	// The same value through the positional channel is syntax, which is the whole
	// difference between the two arguments.
	f, err = ParseWithSubst(`run h:1 $1`, Subst{Pos: []string{"pwd; whoami"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.Statements[0].Payload, "pwd; whoami"; got != want {
		t.Errorf("positional payload = %q, want %q (unquoted syntax)", got, want)
	}
}

// TestPositionalOutsideWholePayloadIsQuoted pins where the raw channel stops.
//
// `run h:1 echo $1` and `run h:1 $1 && echo done` both put the reference beside other
// words, so the caller is naming a WORD in a command line, not the command line. Raw
// splicing there would let args=["make test"] turn one command into two.
func TestPositionalOutsideWholePayloadIsQuoted(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		arg  string
		want string
	}{
		{"as an argument", `run h:1 echo $1`, "a; b", "echo 'a; b'"},
		{"inside double quotes", `run h:1 echo "$1"`, "a; b", `echo "a; b"`},
		{"beside an operator", `run h:1 $1 && echo done`, "make test", "'make test' && echo done"},
		{"as a flag value", `file s --action write --data $1`, "l1\nl2", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseWithSubst(tc.cmd, Subst{Pos: []string{tc.arg}})
			if err != nil {
				t.Fatalf("ParseWithSubst: %v", err)
			}
			st := f.Statements[0]
			if tc.want == "" {
				// A flag directive carries its value in Flags, not in Payload.
				v, ok := st.Flag("data")
				if !ok || v != tc.arg {
					t.Errorf("--data = %q (present %v), want %q", v, ok, tc.arg)
				}
				return
			}
			if st.Payload != tc.want {
				t.Errorf("payload = %q, want %q", st.Payload, tc.want)
			}
		})
	}
}

// TestSubstitutionReachesEveryShellContext pins the coverage fix.
//
// The walker this replaced visited a statement's call, its binary halves and its
// words — and nothing else. A redirect target was never visited, so a `$OUT` defined
// in vars was silently NOT substituted and an undefined one was silently left for the
// shell: one bug, two silent reports, both false. The scan reads the line instead of
// the tree, so the contexts below all resolve.
func TestSubstitutionReachesEveryShellContext(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		sub  Subst
		want string
	}{
		{
			name: "redirect target",
			cmd:  `run h:1 echo hi > $OUT`,
			sub:  Subst{Named: map[string]string{"OUT": "/tmp/a b"}},
			want: `echo hi > '/tmp/a b'`,
		},
		{
			name: "append target",
			cmd:  `run h:1 echo hi >> $OUT`,
			sub:  Subst{Named: map[string]string{"OUT": "log"}},
			want: `echo hi >> 'log'`,
		},
		{
			name: "command substitution body",
			cmd:  `run h:1 echo $(cat $P)`,
			sub:  Subst{Named: map[string]string{"P": "/etc/hosts"}},
			want: `echo $(cat '/etc/hosts')`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseWithSubst(tc.cmd, tc.sub)
			if err != nil {
				t.Fatalf("ParseWithSubst: %v", err)
			}
			if got := f.Statements[0].Payload; got != tc.want {
				t.Errorf("payload = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUndefinedNameInRedirectIsAnError pins the other half of the coverage bug: an
// undefined `$NOPE` in a redirect is now reported like any other, instead of being
// left for the shell to expand into something the caller never chose.
func TestUndefinedNameInRedirectIsAnError(t *testing.T) {
	_, err := ParseWithSubst(`run h:1 echo hi > $NOPE`, Subst{Named: map[string]string{"X": "y"}})
	if err == nil {
		t.Fatal("an undefined $NAME in a redirect target must be an error")
	}
	if !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("error does not name the variable: %v", err)
	}
}

// TestPositionalIndexErrors pins the two refusals: $0 (no such argument) and an
// index past the end. Both are reported before anything runs.
func TestPositionalIndexErrors(t *testing.T) {
	if _, err := ParseWithSubst(`run h:1 $0`, Subst{Pos: []string{"x"}}); err == nil {
		t.Error("$0 must be an error: args is 1-based")
	} else if !strings.Contains(err.Error(), "1-based") {
		t.Errorf("$0 error should explain the indexing: %v", err)
	}

	if _, err := ParseWithSubst(`run h:1 $3`, Subst{Pos: []string{"a", "b"}}); err == nil {
		t.Error("$3 with two args must be an error")
	} else if !strings.Contains(err.Error(), "2 element") {
		t.Errorf("out-of-range error should count the args: %v", err)
	}
}

// TestBraceIndexIsUnambiguous pins the multi-digit rule: `$N` stops at one digit (the
// shell reads `$10` as `$1` then a literal `0`), and `${N}` is how a caller reaches a
// higher argument.
func TestBraceIndexIsUnambiguous(t *testing.T) {
	ten := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "ten"}

	f, err := ParseWithSubst(`run h:1 ${10}`, Subst{Pos: ten})
	if err != nil {
		t.Fatalf("${10}: %v", err)
	}
	if got := f.Statements[0].Payload; got != "ten" {
		t.Errorf("${10} payload = %q, want %q", got, "ten")
	}

	// $10 is $1 followed by a literal 0, and that is correct rather than convenient:
	// it is what the shell does, so a caller reading their own cmd reads the same
	// thing the server does.
	f, err = ParseWithSubst(`run h:1 $10`, Subst{Pos: []string{"ONE"}})
	if err != nil {
		t.Fatalf("$10: %v", err)
	}
	if got, want := f.Statements[0].Payload, "'ONE'0"; got != want {
		t.Errorf("$10 payload = %q, want %q", got, want)
	}
}

// TestNoArgsLeavesShellPositionalAlone pins the namespace rule: `$1` is the shell's
// own positional parameter until the caller supplies args.
//
// Without this, every `awk '{print $1}'` a caller sent would become a 400 the moment
// the args channel existed.
func TestNoArgsLeavesShellPositionalAlone(t *testing.T) {
	f, err := ParseWithSubst(`run h:1 awk '{print $1}'`, Subst{})
	if err != nil {
		t.Fatalf("a shell positional without args must pass through: %v", err)
	}
	if got, want := f.Statements[0].Payload, `awk '{print $1}'`; got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}
}

// TestSentinelCollisionIsTheCallersOwnBytes pins that a substituted value is never
// rescanned: a caller whose argument contains the marker text gets its own bytes
// back, because the fill is single-pass.
func TestSentinelCollisionIsTheCallersOwnBytes(t *testing.T) {
	val := "echo __termcp_0__ here"
	f, err := ParseWithSubst(`run h:1 $1`, Subst{Pos: []string{val}})
	if err != nil {
		t.Fatalf("ParseWithSubst: %v", err)
	}
	if got := f.Statements[0].Payload; got != val {
		t.Errorf("payload = %q, want the caller's own bytes %q", got, val)
	}
}

// TestPathEchoCarriesTheSubstitutedValue pins that the reply's command echo shows
// what ran, not the placeholder that stood in for it.
func TestPathEchoCarriesTheSubstitutedValue(t *testing.T) {
	f, err := ParseWithSubst(`run h:1 $1`, Subst{Pos: []string{"pwd; whoami"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.Statements[0].Path, "run h:1 pwd; whoami"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

// TestRestValueFlagTakesPerWordRaw pins that an argv is assembled from raw words.
//
// `--argv` and `--args` are WORD LISTS that SplitArgv parses a second time, so a
// whole-word `$N` is one element and must arrive unquoted. Quoting it first put the
// quotes into the script itself:
//
//	--argv $1 $2 $3  args ["sh","-c","echo 'a b  c'"]
//	→ argv = "'sh' '-c' 'echo '\''a b  c'\'''"   → the shell printed 'a b c'
//
// Per-word rather than whole-value: an argv is several words, so requiring the
// reference to be the WHOLE value would mean only a single-argument command could
// ever use it.
func TestRestValueFlagTakesPerWordRaw(t *testing.T) {
	f, err := ParseWithSubst(`pipe s --argv $1 $2 $3`,
		Subst{Pos: []string{"sh", "-c", "echo 'a b  c'"}})
	if err != nil {
		t.Fatal(err)
	}
	v, ok := f.Statements[0].Flag("argv")
	if !ok {
		t.Fatal("no argv flag")
	}
	if want := `sh -c echo 'a b  c'`; v != want {
		t.Errorf("argv = %q, want %q", v, want)
	}

	// ext's --args is the same kind of value.
	f, err = ParseWithSubst(`ext s --args $1 $2`, Subst{Pos: []string{"mcp", "serve"}})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Statements[0].Flag("args"); v != "mcp serve" {
		t.Errorf("ext --args = %q, want %q", v, "mcp serve")
	}
}

// TestPerWordRawDoesNotLoosenRun pins that the argv rule did not leak into the
// payload rule: a positional beside an operator is still one quoted word.
func TestPerWordRawDoesNotLoosenRun(t *testing.T) {
	f, err := ParseWithSubst(`run h:1 $1 && echo done`, Subst{Pos: []string{"make test"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.Statements[0].Payload, "'make test' && echo done"; got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}
}

// TestTargetFromArgsIsRaw pins that a target is an IDENTIFIER, not a shell word.
//
// Quoting it produced `info 's:1'`. That happened to still resolve — the resolver
// trims quotes — but the Path echo then showed a spelling nobody wrote, and any
// directive that compared the target as a string would have seen the quotes as part
// of the name.
func TestTargetFromArgsIsRaw(t *testing.T) {
	cases := []struct {
		cmd    string
		target string
		path   string
	}{
		{"info $1", "s:1", "info s:1"},
		{"run $1 echo hi", "s:1", "run s:1 echo hi"},
		{"close $1", "s:1", "close s:1"},
		{"read $1 --tail 1", "s:1", "read s:1 --tail 1"},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			f, err := ParseWithSubst(tc.cmd, Subst{Pos: []string{tc.target}})
			if err != nil {
				t.Fatalf("ParseWithSubst: %v", err)
			}
			st := f.Statements[0]
			if st.Target != tc.target {
				t.Errorf("target = %q, want %q", st.Target, tc.target)
			}
			if st.Path != tc.path {
				t.Errorf("path = %q, want %q", st.Path, tc.path)
			}
		})
	}
}

// TestSubstitutedTargetIsCheckedAfterFilling pins that a target which arrives as
// `args` is still refused when it names the wrong kind of object.
//
// The kind check reads the VALUE, and at parse time a substituted target is the
// sentinel `__termcp_0__` — so the check ran against the placeholder and refused a
// perfectly good handle with `read needs a CHANNEL, not "__termcp_0__"`. Running it
// again after the fill keeps both properties: a literal bad target is refused before
// anything runs, and a substituted one is judged on what it actually says.
func TestSubstitutedTargetIsCheckedAfterFilling(t *testing.T) {
	// A session id where a channel is required: still refused, and now with the
	// caller's own value in the message rather than a sentinel.
	_, err := ParseWithSubst(`read $1 --tail 1`, Subst{Pos: []string{"sess-only"}})
	if err == nil {
		t.Fatal("a session id where a channel is required must be refused")
	}
	if strings.Contains(err.Error(), "__termcp_0__") {
		t.Errorf("the refusal quotes the internal sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "sess-only") {
		t.Errorf("the refusal should name the caller's value: %v", err)
	}

	// And a proper channel is accepted.
	if _, err := ParseWithSubst(`read $1 --tail 1`, Subst{Pos: []string{"s:1"}}); err != nil {
		t.Errorf("a channel handle must be accepted: %v", err)
	}
}
