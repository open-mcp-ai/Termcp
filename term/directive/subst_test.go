package directive

import (
	"fmt"
	"strconv"
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
			name: "input takes a raw payload too",
			cmd:  `input h:1 $1`,
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

// TestAVariableIsAVariableWhicheverTableNamesIt pins the one rule both spellings
// follow: what decides the insertion is the POSITION, not whether the value arrived
// through `args` or `vars`.
//
// They used to differ in exactly one case — a reference that IS the whole payload —
// where `$1` was spliced verbatim and `$CMD` was quoted into a single word. Identical
// text produced a different command line depending on which table named the value, and
// nothing said so. A caller moving a value from one table to the other (a natural edit)
// silently changed what ran.
func TestAVariableIsAVariableWhicheverTableNamesIt(t *testing.T) {
	// One value, two spellings, two legal positions. Every pair must agree: the POSITION
	// decides the insertion, never which table named the value.
	//
	// They used to differ in one case — a reference that IS the whole payload — where
	// `$1` was spliced and `$CMD` was quoted into a single word. Moving a value from one
	// table to the other is a natural edit, and it silently changed what ran.
	const value = "pwd; whoami"
	positions := []struct {
		name string
		cmd  string
		why  string
	}{
		{
			name: "whole payload",
			cmd:  "run h:1 %s",
			why:  "the value IS the command line, so its semicolons are syntax",
		},
		{
			name: "flag value",
			cmd:  "write h file /tmp/x --data %s",
			why:  "no shell reads a flag value, so it is the caller's bytes",
		},
	}
	flagOf := func(st Statement) string { v, _ := st.Flag("data"); return v }
	for _, p := range positions {
		posCmd := fmt.Sprintf(p.cmd, "$1")
		namedCmd := fmt.Sprintf(p.cmd, "$V")
		fp, err := ParseWithSubst(posCmd, Subst{Pos: []string{value}})
		if err != nil {
			t.Fatalf("%s: %v", posCmd, err)
		}
		fn, err := ParseWithSubst(namedCmd, Subst{Named: map[string]string{"V": value}})
		if err != nil {
			t.Fatalf("%s: %v", namedCmd, err)
		}
		sp, sn := fp.Statements[0], fn.Statements[0]
		if sp.Payload != sn.Payload {
			t.Errorf("%s: $1 gave payload %q but $V gave %q — the spelling decided the insertion", p.name, sp.Payload, sn.Payload)
		}
		if gp, gn := flagOf(sp), flagOf(sn); gp != gn {
			t.Errorf("%s: $1 gave --data %q but $V gave %q", p.name, gp, gn)
		}
		if p.name == "whole payload" && sp.Payload != value {
			t.Errorf("%s: %s\ngot  %q\nwant %q", p.name, p.why, sp.Payload, value)
		}
		if p.name == "flag value" {
			if got := flagOf(sp); got != value {
				t.Errorf("%s: %s\ngot  %q\nwant %q", p.name, p.why, got, value)
			}
		}
	}
}

// TestAShellLineIsNeverWrittenAroundAHole is the rule that keeps the two languages
// apart: `cmd` is DSL, a variable's value is the shell line, and a reference that sits
// BESIDE shell text is refused in both spellings.
//
// The refusal is the point. As data the value becomes one quoted word and the caller's
// statement quietly changes; as syntax the caller's DATA becomes commands. There is no
// insertion that leaves the line meaning what it said, so termcp says so instead of
// picking one.
func TestAShellLineIsNeverWrittenAroundAHole(t *testing.T) {
	for _, c := range []struct {
		cmd   string
		named bool
	}{
		{`run h:1 echo $1`, false},
		{`run h:1 echo ${1}`, false},
		{`run h:1 echo "$1"`, false},
		{`run h:1 echo x$1`, false},
		{`run h:1 echo hi > $1`, false},
		{`run h:1 echo $V`, true},
		{`run h:1 echo "$V"`, true},
	} {
		sub := Subst{Pos: []string{"a; b"}}
		if c.named {
			sub = Subst{Named: map[string]string{"V": "a; b"}}
		}
		_, err := ParseWithSubst(c.cmd, sub)
		if err == nil {
			t.Errorf("%s was accepted; a shell line is a value, not a template", c.cmd)
			continue
		}
		if !strings.Contains(err.Error(), "cannot sit inside a shell line") {
			t.Errorf("%s: refusal does not say what is wrong: %v", c.cmd, err)
		}
	}

	// The same reference is fine as the WHOLE payload, and a `&&` still ends the payload:
	// `$1 && notify done` puts the reference alone in its own statement.
	for _, cmd := range []string{`run h:1 $1`, `run h:1 $1 && notify done`} {
		if _, err := ParseWithSubst(cmd, Subst{Pos: []string{"make test"}}); err != nil {
			t.Errorf("%s: %v", cmd, err)
		}
	}
}

// TestPositionalOutsideWholePayloadIsQuoted pins where the raw channel stops.
//
// `run h:1 $1 && notify done` used to be the interesting case (issues #141, #155): the
// `&&` is a directive boundary, so `$1` IS the whole payload of its own statement, and
// quoting it made the terminal run `'make test'` as one program name — the value
// arrived, the reply said 200, and the command had silently stopped being a command line.
func TestPositionalOutsideWholePayloadIsQuoted(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		arg  string
		want string
	}{
		// A CONDITIONAL IS A DIRECTIVE BOUNDARY, so the reference before it is the
		// whole payload of its statement — not a word beside an operator. This is the
		// form a caller writes constantly (`make build && make test` as one arg), and
		// quoting it is what turned a command line into a program name.
		{"before a conditional", `run h:1 $1 && notify done`, "make test", "make test"},
		{"as a flag value", "write s file /tmp/x --data $1", "l1\nl2", ""},
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
				got, _ := st.Flag("data")
				if got != tc.arg {
					t.Errorf("--data = %q, want %q", got, tc.arg)
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
			// THE WHOLE PAYLOAD IS THE REFERENCE, so the value reaches the shell as the
			// command line — including a here-document, which is now just text inside
			// that value rather than a region of the caller's line.
			name: "a here-document travels inside the value",
			cmd:  "run h:1 $1",
			sub:  Subst{Pos: []string{"cat <<EOF\necho C\nEOF"}},
			want: "cat <<EOF\necho C\nEOF",
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

	// THE CONTEXTS THAT ARE NOT HOLES. A redirect target and a command-substitution body
	// are both inside the shell line the caller wrote, so a reference there mixes the two
	// languages and is refused. That is not the old silent miss (where `$OUT` was simply
	// left alone): the caller now hears about it. The redirect belongs in the value:
	// cmd `run h:1 $1`, args ["echo hi > '/tmp/a b'"].
	for _, c := range []struct {
		cmd string
		sub Subst
	}{
		{`run h:1 echo hi > $OUT`, Subst{Named: map[string]string{"OUT": "/tmp/a b"}}},
		{`run h:1 echo hi >> $OUT`, Subst{Named: map[string]string{"OUT": "log"}}},
		{`run h:1 echo $(cat $P)`, Subst{Named: map[string]string{"P": "/etc/hosts"}}},
	} {
		if _, err := ParseWithSubst(c.cmd, c.sub); err == nil {
			t.Errorf("%s was accepted; a redirect target is part of the shell line", c.cmd)
		}
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

	// ONE REFERENCE PER DIRECTIVE, because a reference mixed with shell text is refused —
	// and every value has to be referenced or the unused-value check fires. Ten statements,
	// one reference each, is the shape that satisfies both rules and still proves the digit
	// rule: `${10}` reaches the tenth value, which an unbraced `$10` cannot.
	var cmd strings.Builder
	cmd.WriteString("run h:1 $1")
	for i := 2; i <= 9; i++ {
		cmd.WriteString("; run h:1 $" + strconv.Itoa(i))
	}
	cmd.WriteString("; run h:1 ${10}")
	f, err := ParseWithSubst(cmd.String(), Subst{Pos: ten})
	if err != nil {
		t.Fatalf("${10}: %v", err)
	}
	if len(f.Statements) != 10 {
		t.Fatalf("parsed %d statements, want 10", len(f.Statements))
	}
	for i, st := range f.Statements {
		if got := st.Payload; got != ten[i] {
			t.Errorf("statement %d payload = %q, want %q", i+1, got, ten[i])
		}
	}

	// $10 IS $1 FOLLOWED BY A LITERAL 0 — and that is now an ERROR rather than a silent
	// reading, because it is one word built from two things: a value and a literal. The
	// old behaviour ("ONE" + "0" = "ONE0") was the shell's rule applied to a word the
	// caller wrote in cmd, which is exactly the mixing this grammar refuses. `${10}` is
	// the unambiguous spelling, and it is the one the loop above proves.
	if _, err := ParseWithSubst(`write h file /tmp/x --data $10`, Subst{Pos: []string{"ONE"}}); err == nil {
		t.Error("`$10` glued a value to a literal and was accepted; `${10}` is the spelling for the tenth value")
	}
}

// TestNoArgsLeavesShellPositionalAlone pins the namespace rule: `$1` inside a VALUE is
// the shell's own positional parameter, not ours.
//
// This is the case the rule protects: a caller whose command line contains `$1` — an
// awk program, a shell function — must be able to pass it as a value. The reference in
// cmd is ours and the ones inside the value are the shell's, and the two never meet
// because a substituted value is never rescanned.
func TestNoArgsLeavesShellPositionalAlone(t *testing.T) {
	f, err := ParseWithSubst(`run h:1 $1`, Subst{Pos: []string{`awk '{print $1}'`}})
	if err != nil {
		t.Fatalf("a shell positional inside a value must pass through: %v", err)
	}
	if got, want := f.Statements[0].Payload, `awk '{print $1}'`; got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}
	// With no args at all, `$1` is the shell's own parameter — the payload is still one
	// reference, and nothing here may substitute it.
	if f, err := ParseWithSubst(`run h:1 $1`, Subst{}); err != nil {
		t.Fatalf("a bare reference with no table must pass through: %v", err)
	} else if got := f.Statements[0].Payload; got != "$1" {
		t.Errorf("payload = %q, want $1 untouched", got)
	}
}

// TestSentinelCollisionIsTheCallersOwnBytes pins that a substituted value is never
// rescanned: a caller whose argument contains the marker text gets its own bytes
// back, because the fill is single-pass.
func TestSentinelCollisionIsTheCallersOwnBytes(t *testing.T) {
	val := "echo __hole_deadbeefcafe__ here"
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
	f, err := ParseWithSubst(`connect s --argv $1 $2 $3`,
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

	// create ext's --args is the same kind of value.
	f, err = ParseWithSubst(`create ext s --args $1 $2`, Subst{Pos: []string{"mcp", "serve"}})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Statements[0].Flag("args"); v != "mcp serve" {
		t.Errorf("create ext --args = %q, want %q", v, "mcp serve")
	}
}

// TestPerWordRawDoesNotLoosenRun pins that the argv rule did not leak into the
// payload rule, while keeping the one case where a positional IS the payload.
//
// `$1` alone before a `&&` is the whole payload of the first directive, so it goes
// in raw (issues #141, #155). What must NOT happen is the argv rule — where every
// whole word may be a reference — leaking into a command line: `echo $1` still
// quotes, because there the caller named a word.
func TestPerWordRawDoesNotLoosenRun(t *testing.T) {
	f, err := ParseWithSubst(`run h:1 $1 && notify done`, Subst{Pos: []string{"make test"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.Statements[0].Payload, "make test"; got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}
	// The narrowing that matters: a reference BESIDE other words is not a hole at all,
	// so the argv rule cannot reach it and nothing is spliced.
	if _, err := ParseWithSubst(`run h:1 echo $1`, Subst{Pos: []string{"make test"}}); err == nil {
		t.Error("a reference beside shell text was accepted; DSL and shell are not mixed")
	}
	if len(f.Statements) != 2 || f.Statements[1].Cond != "&&" {
		t.Fatalf("`&&` did not become a directive operator: %+v", f.Statements)
	}
	if got := f.Statements[1].Op; got != "notify" {
		t.Errorf("second statement = %q, want notify", got)
	}
}

// TestTargetFromArgsIsRaw pins that a target is an IDENTIFIER, not a shell word.
//
// Quoting it produced `get 's:1'`. That happened to still resolve — the resolver
// trims quotes — but the Path echo then showed a spelling nobody wrote, and any
// directive that compared the target as a string would have seen the quotes as part
// of the name.
func TestTargetFromArgsIsRaw(t *testing.T) {
	cases := []struct {
		cmd    string
		target string
		args   []string
		path   string
	}{
		{"get $1", "s:1", []string{"s:1"}, "get s:1"},
		{"run $1 $2", "s:1", []string{"s:1", "echo hi"}, "run s:1 echo hi"},
		{"close $1", "s:1", []string{"s:1"}, "close s:1"},
		{"read $1 --tail 1", "s:1", []string{"s:1"}, "read s:1 --tail 1"},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			f, err := ParseWithSubst(tc.cmd, Subst{Pos: tc.args})
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
