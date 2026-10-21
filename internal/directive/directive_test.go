package directive

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestPayloadIsSlicedNotReparsed pins the rule the whole format depends on: the
// payload is a SLICE OF THE SOURCE, so quoting, escaping and `$()` reach the shell
// exactly as the caller wrote them.
//
// The case that makes it a hard requirement is `'a'"b"'c'`: three shell parts in
// one word. Re-joining the parsed words yields `abc`, which is a different command
// line. Slicing yields the original.
func TestPayloadIsSlicedNotReparsed(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{
			name: "a pipeline with quotes and && survives byte for byte",
			cmd:  `run a1f3:1 ls -la | grep -c '\.go$' && echo "rc=$?"`,
			want: `ls -la | grep -c '\.go$' && echo "rc=$?"`,
		},
		{
			name: "adjacent quoted parts are not merged",
			cmd:  `run a1f3:1 'a'"b"'c'`,
			want: `'a'"b"'c'`,
		},
		{
			name: "an unquoted multi-word payload is not re-quoted",
			cmd:  `run a1f3:1 echo a b`,
			want: `echo a b`,
		},
		{
			name: "dollar constructs are passed through, not expanded",
			cmd:  `run a1f3:1 'echo $HOME'`,
			want: `'echo $HOME'`,
		},
		{
			name: "backslashes survive",
			cmd:  `run a1f3:1 echo a\ b`,
			want: `echo a\ b`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Parse(tc.cmd)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(f.Statements) != 1 {
				t.Fatalf("statements = %d, want 1", len(f.Statements))
			}
			if got := f.Statements[0].Payload; got != tc.want {
				t.Errorf("payload = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOnlyParseNeverEvaluate pins that a payload is not interpreted: the parser
// does not expand $HOME, $(), or backticks, because expansion is the shell's job
// and doing it here would break the zero-rewrite guarantee.
func TestOnlyParseNeverEvaluate(t *testing.T) {
	f, err := Parse(`run a1f3:1 'echo $HOME $(date) ` + "`id`" + `'`)
	if err != nil {
		t.Fatal(err)
	}
	payload := f.Statements[0].Payload
	for _, want := range []string{"$HOME", "$(date)", "`id`"} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload %q lost %q: the parser must not evaluate", payload, want)
		}
	}
}

// TestFlagValueIsByteFaithful pins that a flag VALUE is the caller's own bytes with
// only the quoting removed. This is issue #131's P0-1, and the same defect in one
// place explains every silent rewrite in that report:
//
//	--data "A`core`B"    -> "AB"          (the subst vanished)
//	--data "$HOME/x"     -> "/x"          (the expansion vanished)
//	--data $B            -> "${B}"        (the parser rewrote it)
//	--data "$((1+2))"    -> ""            (the value did not exist)
//	--data "a\r\nb"      -> "a\nb"        (the CR was dropped)
//
// The mechanism is that syntax's per-node Value is a model of what a shell would
// EXPAND, not a copy of what the caller wrote, and the old reader used it for
// double-quoted content while inventing text (partText) for everything else. The
// fix reads Pos()/End() slices, which are byte-faithful by construction.
//
// A payload was never affected — it is sliced whole — which is why `run` looked
// fine while `--data` silently mangled all of the above.
func TestFlagValueIsByteFaithful(t *testing.T) {
	CR, LF, BS := "\r", "\n", "\\"
	cases := []struct {
		name string
		word string // the flag's value exactly as written in the command
		want string
	}{
		{"backtick substitution", `"A` + "`core`" + `B"`, "A`core`B"},
		{"dollar-paren substitution", `"$(date)"`, "$(date)"},
		{"parameter expansion", `"$HOME/x"`, "$HOME/x"},
		{"bare parameter expansion", `$B`, "$B"},
		{"arithmetic expansion", `"$((1+2))"`, "$((1+2))"},
		{"single quotes protect everything", `'A` + "`core`" + `$B'`, "A`core`$B"},
		{"quoted CRLF keeps its CR", `"a` + CR + LF + `b"`, "a" + CR + LF + "b"},
		{"single-quoted CRLF keeps its CR", `'a` + CR + LF + `b'`, "a" + CR + LF + "b"},
		{"quoted LF", `"a` + LF + `b"`, "a" + LF + "b"},
		// A backslash is NOT decoded: `--data "a\nb"` writes the four bytes a, \, n, b,
		// which is the documented way to put a newline in a --mode text payload, and it
		// is what the old reader did too. Decoding escapes here would make the working
		// channel in #131 stop working.
		{"backslash escapes are NOT decoded", `"a` + BS + `nb"`, "a" + BS + "nb"},
		{"escaped quote keeps its backslash", `"a` + BS + `"b"`, "a" + BS + `"b`},
		{"adjacent quoted parts are joined", `"a"'b'`, "ab"},
		{"mixed literal and quoted", `x"y"'z'`, "xyz"},
		{"empty string is an explicit value", `""`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Parse(`file h --action write --data ` + tc.word)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			got, ok := f.Statements[0].Flag("data")
			if !ok {
				t.Fatalf("--data was not given a value (word %q)", tc.word)
			}
			if got != tc.want {
				t.Errorf("--data %s\n  value = %q\n  want  %q", tc.word, got, tc.want)
			}
		})
	}
}

// TestFlagWithNoValueIsNotAnEmptyValue pins the three states the flag map carries.
//
// They used to be one: `--remote-path` with nothing after it and `--remote-path`
// followed by an empty quoted value both arrived as (present, ""), so a reader
// could not tell a caller who meant an empty string from one whose value had been
// eaten by a quoting slip, and neither could be reported by name. `--data` with an
// empty value writing an empty file is legitimate; `--remote-path` with no value at
// all is always a mistake.
func TestFlagWithNoValueIsNotAnEmptyValue(t *testing.T) {
	for _, tc := range []struct {
		cmd   string
		flag  string
		state FlagValue
		value string
		has   bool
	}{
		{"file h --data ''", "data", FlagValueSet, "", true},
		{`file h --data ""`, "data", FlagValueSet, "", true},
		{"file h --data=", "data", FlagValueSet, "", true},
		{"file h --data", "data", FlagNoValue, "", false},
		{"file h --action read", "data", FlagAbsent, "", false},
		// A valueless flag is SET: `--raw` needs no value and means the flag was
		// written, which is the whole question it asks.
		{"read h:1 --raw", "raw", FlagValueSet, "", true},
	} {
		f, err := Parse(tc.cmd)
		if err != nil {
			t.Fatalf("%q: %v", tc.cmd, err)
		}
		st := f.Statements[0]
		if got := st.FlagState(tc.flag); got != tc.state {
			t.Errorf("%q: --%s state = %v, want %v", tc.cmd, tc.flag, got, tc.state)
		}
		if v, ok := st.Flag(tc.flag); ok != tc.has || (tc.has && v != tc.value) {
			t.Errorf("%q: Flag(%q) = %q,%v; want %q,%v", tc.cmd, tc.flag, v, ok, tc.value, tc.has)
		}
	}
}

// TestSplitArgvIsByteFaithful extends the same guarantee to --argv, which a pipe or
// an extension uses to start a program. Its words go through wordLiteral too, so an
// argument containing a backtick or a quoted CRLF must arrive as typed.
func TestSplitArgvIsByteFaithful(t *testing.T) {
	CR, LF := "\r", "\n"
	cmd, args, err := SplitArgv(`sh -c 'echo ` + "`id`" + `' 'a` + CR + LF + `b'`)
	if err != nil {
		t.Fatalf("SplitArgv: %v", err)
	}
	if cmd != "sh" {
		t.Errorf("command = %q, want sh", cmd)
	}
	if len(args) != 3 || args[0] != "-c" {
		t.Fatalf("args = %q, want [-c _ _]", args)
	}
	if want := "echo `id`"; args[1] != want {
		t.Errorf("script = %q, want %q", args[1], want)
	}
	if want := "a" + CR + LF + "b"; args[2] != want {
		t.Errorf("quoted CRLF = %q, want %q", args[2], want)
	}
}

// TestHashIsNotATargetPrefix pins the correction that produced the current
// syntax. `#` starts a comment in the shell grammar, so `run #a1f3:1 'x'` parses
// to a single word. That must be an ERROR with a usage line, never a silent
// no-op or an attempt to "fix" it.
func TestHashIsNotATargetPrefix(t *testing.T) {
	_, err := Parse(`run #a1f3:1 'x'`)
	if err == nil {
		t.Fatal("`run #a1f3:1` must fail: '#' is a comment, so the target is gone")
	}
	var derr *Error
	if !asError(err, &derr) {
		t.Fatalf("error is %T, want *directive.Error", err)
	}
	if !strings.Contains(derr.Msg, "needs a target") {
		t.Errorf("message = %q, want it to say the target is missing", derr.Msg)
	}
	if derr.Usage == "" {
		t.Error("the failure must carry a usage line, so a caller learns the shape")
	}
}

// TestQuotedTargetNamesTheSameThing pins that quoting is not part of a target: it
// is what the shell strips before the directive sees the word. Quoting is also the
// only way to write a word-initial '#', so this is the spelling `info '#sid'` — the
// one a caller reaches for when the target is a `#<session>` locator — must not
// lose its target.
//
// The regression it locks: the quoted form used to parse to the EMPTY string (a
// source-slice reader refused the quote, its caller dropped the refusal), so the
// reply was `Session ” not found` with nothing pointing at the quotes.
func TestQuotedTargetNamesTheSameThing(t *testing.T) {
	for _, cmd := range []string{
		`info a1f3:1`,
		`info 'a1f3:1'`,
		`info "a1f3:1"`,
	} {
		f, err := Parse(cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if got := f.Statements[0].Target; got != "a1f3:1" {
			t.Errorf("%s: target = %q, want %q", cmd, got, "a1f3:1")
		}
	}
}

// TestLocatorSpellings pins both accepted target spellings: the short form
// (which every reply emits) and the old termcp:// form (which the Web UI's copy
// button still produces). The `#` inside a word is not a comment.
func TestLocatorSpellings(t *testing.T) {
	for _, cmd := range []string{`run a1f3:1 'x'`, `run termcp://#a1f3:1 'x'`} {
		f, err := Parse(cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if got := f.Statements[0].Target; got != strings.Fields(cmd)[1] {
			t.Errorf("%s: target = %q", cmd, got)
		}
	}
}

// TestBatchTargetIsOneWord pins that a comma-separated target is a single word:
// the shell does not split on commas, so a batch needs no new syntax.
func TestBatchTargetIsOneWord(t *testing.T) {
	f, err := Parse(`run a1f3:1,b7c2:1 'x'`)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Target; got != "a1f3:1,b7c2:1" {
		t.Errorf("target = %q, want the whole comma list as one word", got)
	}
}

// TestFlagsComeAfterTheTarget pins the positional rule: word 2 is the target, so
// a flag in second position is refused rather than read as a target.
func TestFlagsComeAfterTheTarget(t *testing.T) {
	f, err := Parse(`key a1f3:1 up --repeat 3`)
	if err != nil {
		t.Fatal(err)
	}
	st := f.Statements[0]
	if st.Target != "a1f3:1" {
		t.Errorf("target = %q", st.Target)
	}
	if len(st.Args) != 1 || st.Args[0] != "up" {
		t.Errorf("args = %v, want [up]", st.Args)
	}
	if got, want := flagOf(t, st, "repeat"), "3"; got != want {
		t.Errorf("repeat = %q, want %q", got, want)
	}

	if _, err := Parse(`key --repeat 3 a1f3:1 up`); err == nil {
		t.Fatal("a flag before the target must be refused: it would silently become the target")
	} else if !strings.Contains(err.Error(), "after the target") {
		t.Errorf("error = %v, want it to say flags come after the target", err)
	}
}

// TestArgvTakesTheRestOfTheStatement pins that --argv is a word list: the words
// after it belong to the program, so flags must come before it and a bare
// payload is refused rather than dropped.
func TestArgvTakesTheRestOfTheStatement(t *testing.T) {
	f, err := Parse(`pipe internal --name chat --argv sh -c 'echo hi'`)
	if err != nil {
		t.Fatal(err)
	}
	st := f.Statements[0]
	if got, want := flagOf(t, st, "argv"), `sh -c 'echo hi'`; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if got := flagOf(t, st, "name"); got != "chat" {
		t.Errorf("name = %q", got)
	}
	if st.Payload != "" {
		t.Errorf("payload = %q; --argv consumed it", st.Payload)
	}

	if _, err := Parse(`pipe internal --argv echo hi --name chat`); err == nil {
		t.Fatal("a known flag after --argv must be refused, not passed to the program")
	} else if !strings.Contains(err.Error(), "flags come before --argv") {
		t.Errorf("error = %v", err)
	}

	if _, err := Parse(`pipe internal sleep 5`); err == nil {
		t.Fatal("a bare payload must be refused, not silently dropped")
	} else if !strings.Contains(err.Error(), "--argv") {
		t.Errorf("error = %v, want it to name --argv", err)
	}
}

// TestSplitArgvReadsWordsNotSpaces pins the --argv splitting rule: words are
// read with the shell's quoting rules, exactly as a terminal would read them.
// There is no compatibility rule for a whole-quoted value — quoting the whole
// argv makes it one word, because that is what the terminal does.
func TestSplitArgvReadsWordsNotSpaces(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantCmd string
		want    []string
	}{
		{"unquoted words", "echo hello", "echo", []string{"hello"}},
		{"a quoted argument keeps its space", `sh -c "echo hello"`, "sh", []string{"-c", "echo hello"}},
		{"a quoted script is one word", `sh -c 'cd /srv && make'`, "sh", []string{"-c", "cd /srv && make"}},
		{"the whole argv quoted is one word", `'go build ./...'`, "go build ./...", nil},
		{"a quoted program name stays one word", `'sleep 5'`, "sleep 5", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, args, err := SplitArgv(tc.raw)
			if err != nil {
				t.Fatalf("SplitArgv(%q): %v", tc.raw, err)
			}
			if cmd != tc.wantCmd || !slices.Equal(args, tc.want) {
				t.Errorf("SplitArgv(%q) = %q %v, want %q %v", tc.raw, cmd, args, tc.wantCmd, tc.want)
			}
		})
	}
}

// TestSplitArgvRefusesACommandLine pins that shell syntax is an error rather
// than text handed to the program: --argv is an argv, not a command line.
func TestSplitArgvRefusesACommandLine(t *testing.T) {
	for _, raw := range []string{
		`echo hi; ls`,
		`echo hi | wc`,
		`echo hi > /tmp/x`,
		`FOO=1 echo hi`,
		`echo "unbalanced`,
	} {
		if _, _, err := SplitArgv(raw); err == nil {
			t.Errorf("SplitArgv(%q) accepted a command line", raw)
		}
	}
}

// TestTargetOptionalFlagForms pins `watch --off <rule>` and `watch --list`:
// the documented flag-only forms have no target, so the positional rule must
// not read `--off` as one.
func TestTargetOptionalFlagForms(t *testing.T) {
	f, err := Parse(`watch --off notif_1`)
	if err != nil {
		t.Fatal(err)
	}
	st := f.Statements[0]
	if st.Target != "" {
		t.Errorf("target = %q, want empty for --off", st.Target)
	}
	if got := flagOf(t, st, "off"); got != "notif_1" {
		t.Errorf("off = %q", got)
	}

	f, err = Parse(`watch --list`)
	if err != nil {
		t.Fatal(err)
	}
	if !boolFlagSet(f.Statements[0], "list") {
		t.Error("--list must be present")
	}

	f, err = Parse(`watch a1f3:1 --event output`)
	if err != nil {
		t.Fatal(err)
	}
	if got := flagOf(t, f.Statements[0], "event"); f.Statements[0].Target != "a1f3:1" || got != "output" {
		t.Errorf("target/event = %q/%q", f.Statements[0].Target, got)
	}
}

// TestFlagWithValue pins both spellings of a valued flag.
func TestFlagWithValue(t *testing.T) {
	f, err := Parse(`read a1f3:1 --cursor 1024 --stream o --raw`)
	if err != nil {
		t.Fatal(err)
	}
	st := f.Statements[0]
	if got := flagOf(t, st, "cursor"); got != "1024" {
		t.Errorf("cursor = %q", got)
	}
	if !boolFlagSet(st, "raw") {
		t.Error("a valueless flag must still be present: 'was it set' and 'what to' are two questions")
	}
	if got := flagOf(t, st, "stream"); got != "o" {
		t.Errorf("stream = %q", got)
	}

	f, err = Parse(`read a1f3:1 --cursor=2048`)
	if err != nil {
		t.Fatal(err)
	}
	if got := flagOf(t, f.Statements[0], "cursor"); got != "2048" {
		t.Errorf("--cursor=2048 parsed as %q", got)
	}
}

// flagOf returns a flag's value, failing the test when the flag was absent or
// written without one. Nearly every flag in the grammar takes a value, so this is
// the reading the tests want; the three-state distinction has its own test below.
func flagOf(t *testing.T, st Statement, name string) string {
	t.Helper()
	v, ok := st.Flag(name)
	if !ok {
		t.Fatalf("flag --%s was not given a value (state %v)", name, st.FlagState(name))
	}
	return v
}

// boolFlagSet reports whether a valueless flag was WRITTEN at all.
func boolFlagSet(st Statement, name string) bool {
	return st.FlagState(name) != FlagAbsent
}

// TestUnknownDirectiveAndFlag pins the two failures learn_mode turns into a
// Usage line: a name that is not a directive, and a flag that is not that
// directive's.
func TestUnknownDirectiveAndFlag(t *testing.T) {
	if _, err := Parse(`raed a1f3:1 'x'`); err == nil {
		t.Fatal("an unknown directive must fail")
	} else if !strings.Contains(err.Error(), "unknown directive") {
		t.Errorf("error = %v", err)
	}

	if _, err := Parse(`read a1f3:1 --bogus 1`); err == nil {
		t.Fatal("an unknown flag must fail")
	} else if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error = %v", err)
	}
}

// TestStatementsSplitOnSemicolonNotOnAnd pins the sequencing rule: `;` asks two
// questions and gets two slots, while `&&` stays inside one payload because it is
// one command whose result the caller wants.
func TestStatementsSplitOnSemicolonNotOnAnd(t *testing.T) {
	f, err := Parse(`run a1f3:1 'x'; run b7c2:1 'y'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Statements) != 2 {
		t.Fatalf("statements = %d, want 2", len(f.Statements))
	}
	if f.Statements[0].Target != "a1f3:1" || f.Statements[1].Target != "b7c2:1" {
		t.Errorf("targets = %q, %q", f.Statements[0].Target, f.Statements[1].Target)
	}

	f, err = Parse(`run a1f3:1 make && echo ok`)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Statements) != 1 {
		t.Fatalf("`&&` split into %d statements; it is one command line", len(f.Statements))
	}
	if got := f.Statements[0].Payload; got != "make && echo ok" {
		t.Errorf("payload = %q", got)
	}
}

// TestEmptyIsLegal pins that an empty cmd is the "where am I" call: legal and
// cheap, never an error.
func TestEmptyIsLegal(t *testing.T) {
	for _, cmd := range []string{"", "   ", "\n"} {
		f, err := Parse(cmd)
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		if !f.Empty {
			t.Errorf("%q parsed as non-empty", cmd)
		}
	}
}

// TestPathIsTheCommandEcho pins the Path header: the caller's own bytes for that
// statement, so a combined command can be reconciled against what came back.
func TestPathIsTheCommandEcho(t *testing.T) {
	cmd := `run a1f3:1 'test -f go.mod && go test ./... || echo no-module'`
	f, err := Parse(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Path; got != cmd {
		t.Errorf("Path = %q, want %q", got, cmd)
	}
}

// TestEveryDirectiveParsesItsOwnUsage keeps the table honest: every FORM a usage
// line documents must be a line this parser accepts. A usage that does not parse
// teaches a shape the tool cannot run — which is how `watch --off <rule>` stayed
// documented and unparseable until a live caller tried it.
func TestEveryDirectiveParsesItsOwnUsage(t *testing.T) {
	for _, op := range Ops() {
		for _, form := range usageAlternatives.Split(op.Usage, -1) {
			line := fillUsageStubs(stripOptionals(form))
			if line == "" || strings.Contains(line, "…") {
				continue // prose, not a template
			}
			// A documented form starts with the directive's own name; anything
			// else after `|` is an explanation (`payload: …`, `只打字，不回车`).
			if line != op.Name && !strings.HasPrefix(line, op.Name+" ") {
				continue
			}
			if _, err := Parse(line); err != nil {
				t.Errorf("%s: usage form %q parses as %q, which fails: %v",
					op.Name, strings.TrimSpace(form), line, err)
			}
		}
	}
}

// usageAlternatives splits a usage line into its documented forms. The spaces
// are load-bearing: `|` inside a placeholder (<handle|session>) is not an
// alternative separator.
var usageAlternatives = regexp.MustCompile(`\s+\|\s+`)

// stripOptionals removes every bracketed group, nested ones included. Optional
// parts may be omitted, so what remains must parse on its own.
func stripOptionals(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// fillUsageStubs replaces every <placeholder> with a dummy word and normalizes
// spaces, so the line is something the parser can read.
// fillUsageStubs replaces every <placeholder> with a value this grammar accepts.
//
// A `<handle>` becomes `x:1`, not `x`, and that is not cosmetic: a handle is a
// CHANNEL (the session id plus a channel index), and a channel-only directive
// refuses a bare session id at parse time. Filling it with `x` would make this test
// assert that the documented shape does not parse — the opposite of its job.
func fillUsageStubs(s string) string {
	for {
		start := strings.IndexByte(s, '<')
		if start < 0 {
			break
		}
		end := strings.IndexByte(s[start:], '>')
		if end < 0 {
			break
		}
		stub := "x"
		if strings.Contains(s[start:start+end+1], "handle") {
			stub = "x:1"
		}
		s = s[:start] + stub + s[start+end+1:]
	}
	return strings.Join(strings.Fields(s), " ")
}

// asError is errors.As without the import, kept local so the test reads plainly.
func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

// TestTrailingSemicolonIsNotPayload pins the fix for a `;` leaking into the
// caller's bytes.
//
// Statements are separated by `;`, so `run h echo hi;` is ONE statement whose
// text ends at `echo hi`. The parser sliced payloads to stmt.End(), which counts
// the separator, so the shell received `echo hi;` — and worse, a trailing `;` on
// a flag value or a handle produced `mcp tools a1f3:1;` → "shell 'a1f3:1;' not
// found", and `alias x3 help read;` stored `x3 = help read;` instead of a
// directive.
func TestTrailingSemicolonIsNotPayload(t *testing.T) {
	f, err := Parse("run a1f3:1 echo AAA;")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Statements) != 1 {
		t.Fatalf("statements = %d, want 1", len(f.Statements))
	}
	if got := f.Statements[0].Payload; got != "echo AAA" {
		t.Errorf("payload = %q, want %q", got, "echo AAA")
	}
	if got := f.Statements[0].Path; got != "run a1f3:1 echo AAA" {
		t.Errorf("path = %q", got)
	}

	// Same for a payload whose own text legitimately ends in punctuation, and for
	// the two statements of a chain: each keeps its own bytes, neither keeps the
	// separator.
	f, err = Parse("run a1f3:1 echo A; run b:1 echo B;")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Statements) != 2 {
		t.Fatalf("statements = %d, want 2", len(f.Statements))
	}
	if got := f.Statements[0].Payload; got != "echo A" {
		t.Errorf("stmt0 payload = %q", got)
	}
	if got := f.Statements[1].Payload; got != "echo B" {
		t.Errorf("stmt1 payload = %q", got)
	}

	// A quoted `;` belongs to the caller and must survive.
	f, err = Parse(`run a1f3:1 echo 'a;b';`)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Payload; got != `echo 'a;b'` {
		t.Errorf("quoted semicolon payload = %q", got)
	}

	// --argv takes the rest of the statement: the separator stays out of the
	// program's last argument.
	f, err = Parse(`pipe mac --argv printf '[%s]' 'a';`)
	if err != nil {
		t.Fatal(err)
	}
	if got := flagOf(t, f.Statements[0], "argv"); got != `printf '[%s]' 'a'` {
		t.Errorf("argv = %q", got)
	}
}

// TestFwdTakesItsSessionAsTheTarget pins the positional form the usage line
// advertises: `fwd <session> --action …`.
//
// The op was declared without Target, so the second word was never read as the
// object: `fwd a1f3 --action dynamic` left stmt.Target empty and the forward tool
// answered "session_id required" for a command that had named its session. The
// only working spelling was the undocumented `--session`, which the usage line
// does not mention — a documented form that cannot work and a working form that
// is not documented.
func TestFwdTakesItsSessionAsTheTarget(t *testing.T) {
	f, err := Parse("fwd a1f3 --action dynamic --local-port 11080")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Statements) != 1 {
		t.Fatalf("got %d statements, want 1", len(f.Statements))
	}
	st := f.Statements[0]
	if st.Target != "a1f3" {
		t.Errorf("Target = %q, want %q", st.Target, "a1f3")
	}
	if got := flagOf(t, st, "action"); got != "dynamic" {
		t.Errorf("action = %q, want %q", got, "dynamic")
	}
	if got := flagOf(t, st, "local-port"); got != "11080" {
		t.Errorf("local-port = %q, want %q", got, "11080")
	}

	// The flag form still works: it is the one existing callers may be using.
	f, err = Parse("fwd --session a1f3 --action list")
	if err != nil {
		t.Fatal(err)
	}
	if got := flagOf(t, f.Statements[0], "session"); got != "a1f3" {
		t.Errorf("--session = %q, want %q", got, "a1f3")
	}
}

// TestEveryFlagIsDocumented pins the flag table against the help text.
//
// A flag that exists but appears in no usage line and no Detail page is a flag
// only the source knows about, which is the same as not having it: the caller
// who needs `file --local-offset` reads `help file`, does not find it, and
// concludes the capability is missing. The reverse is worse and is caught in the
// mcp package, where a flag can be listed here and consumed nowhere.
func TestEveryFlagIsDocumented(t *testing.T) {
	for _, op := range Ops() {
		if len(op.Flags) == 0 {
			continue
		}
		// Usage may name flags; Detail is where the ones that do not fit belong.
		documented := op.Usage + "\n" + op.Detail
		var missing []string
		for _, f := range op.Flags {
			if !strings.Contains(documented, "--"+f) {
				missing = append(missing, "--"+f)
			}
		}
		if len(missing) > 0 {
			t.Errorf("%s: %d flag(s) documented nowhere (neither Usage nor Detail): %s\n"+
				"add them to the Detail page, or drop them from Flags if nothing consumes them",
				op.Name, len(missing), strings.Join(missing, ", "))
		}
	}
}

// TestValuelessFlagsAreReal pins that a flag listed in Valueless is also in
// Flags: the parser consults them separately, so an entry in one and not the
// other is either a flag that can never be set or one that silently demands a
// value it does not take.
func TestValuelessFlagsAreReal(t *testing.T) {
	for _, op := range Ops() {
		for _, v := range op.Valueless {
			if !slices.Contains(op.Flags, v) {
				t.Errorf("%s: %q is Valueless but not in Flags", op.Name, v)
			}
		}
	}
}

// TestExtArgsTakesTheRestOfTheLine pins --args to the same grammar --argv uses,
// and pins that it takes the rest of the statement.
//
// --args used to be comma-split by the server, which cannot express an argv: an
// argument may contain a comma, and `sh -c 'a,b'` became two arguments. The
// failure was silent — the extension started with a mangled argv and reported a
// startup error that named no flag — so the shape is pinned here, where the
// parser can refuse it before anything runs.
func TestExtArgsTakesTheRestOfTheLine(t *testing.T) {
	f, err := Parse(`ext create srv --kind mcp --target internal --mode pipe --command node --args /srv/mcp.js --stdio --label 'a,b'`)
	if err != nil {
		t.Fatal(err)
	}
	st := f.Statements[0]
	want := `/srv/mcp.js --stdio --label 'a,b'`
	if got := flagOf(t, st, "args"); got != want {
		t.Errorf("args = %q, want %q (the rest of the line, quoting intact)", got, want)
	}
	// The other flags still parsed: --args stops the scan, it does not swallow
	// what came before it.
	for k, want := range map[string]string{"kind": "mcp", "target": "internal", "mode": "pipe", "command": "node"} {
		if got := flagOf(t, st, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// A known flag after --args is the caller's mistake, not a program argument.
	if _, err := Parse(`ext create srv --args mcp --name srv`); err == nil {
		t.Fatal("a flag after --args must be refused, not handed to the program")
	} else if !strings.Contains(err.Error(), "flags come before --args") {
		t.Errorf("error = %v, want it to name the ordering rule", err)
	}
}

// TestExtensionsIsABuiltinSpellingOfExt pins the synonym.
//
// A caller reaches for `extensions` before reading help — it is the tool's own
// name for the thing — and an unknown-directive error at that moment is the
// documentation failing, not the caller. The synonym resolves to the canonical
// name, so every downstream lookup (dispatch, capability) sees one spelling.
func TestExtensionsIsABuiltinSpellingOfExt(t *testing.T) {
	if _, ok := Lookup("extensions"); !ok {
		t.Fatal("`extensions` must resolve; it is the tool's own name for `ext`")
	}
	f, err := Parse(`extensions list`)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Op; got != "ext" {
		t.Errorf("op = %q, want the canonical %q", got, "ext")
	}
	// help lists canonical names only: a synonym is a way in, not a second entry.
	for _, name := range Names() {
		if name == "extensions" {
			t.Error("`extensions` must not appear as its own help entry")
		}
	}
}
