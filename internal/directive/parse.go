// Package directive turns the single-entry `term` tool's `cmd` string into
// statements: which directive, which target, which flags, and — the part that
// must not be re-derived anywhere — the payload, taken as a SLICE OF THE SOURCE
// rather than re-assembled from parsed words.
//
// It parses with the real shell grammar (mvdan.cc/sh/v3/syntax) and never
// evaluates: `$HOME` in a payload is not expanded here, it is handed to the
// shell that will expand it. The rule that makes the whole format work is the
// one in the issue that produced this package: every word in `cmd` must be a word
// the shell itself can parse. `#` is a comment character, so a locator can never
// start with one — `run #a1f3:1 'x'` parses to just `run`, and that is reported
// as an error rather than silently fixed up.
//
// Two shapes exist, and the difference is a property of the directive:
//
//	payload  (run, alias):        everything after the target is raw bytes
//	flags    (key, read, file, …): --name[=value] after the target
//
// `pipe`/`pty` are payload directives whose `--argv` flag takes the REST of the
// statement: an argv is a word list (`--argv sh -c 'echo hi'`), and reading only
// the word after the flag would silently drop the program's arguments.
//
// Flags go AFTER the target, never before: `key --repeat 3 a1f3:1 up` is
// rejected, because a flag in second position breaks the one positional rule the
// whole syntax rests on ("word 2 is the target").
package directive

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Parse reads cmd into statements.
//
// Statements are separated by `;` and newlines — the shell's own sequencing —
// while `&&`, `||` and `|` stay INSIDE a statement's payload. That split is
// deliberate: `run h make && echo ok` is one command whose result the caller
// wants (and one reply slot), whereas `run a:1 'x'; run b:1 'y'` asks two
// questions and gets two slots.
func Parse(src string) (*File, error) {
	return ParseWithSubst(src, Subst{})
}

// ParseWithSubst reads cmd into statements, with the caller's values filled in at
// the two places they can appear: shell syntax, and the directive's own arguments.
//
// WHY SUBSTITUTE AT ALL. A caller that wants to send a payload containing a quote,
// a newline, a semicolon or a carriage return has no way to write it into cmd: every
// one of those either ends the statement or is re-interpreted by the shell that
// eventually receives it. Escaping it is a guessing game about which layer will
// decode it. The table is the way out — the value never appears in the command
// text, so nothing in the command text can mangle it.
//
// It is NOT a process environment: nothing here is exported to the child, a key the
// caller supplies but cmd never mentions is ignored, and a name cmd uses that the
// table does not define is an error rather than an empty expansion.
//
// THE TWO ROLES ARE DIFFERENT AND BOTH ARE NEEDED. A named value (vars) becomes one
// shell WORD, because it is data: `--data $BODY` must not let a newline in BODY
// become a second command. A positional value (args) becomes shell SYNTAX inside a
// payload, because it is a command line: `run h:1 $1` with `pwd; whoami` must run
// two commands, not look for a program called `pwd; whoami`. See Subst.
//
// The substitution is a NODE REPLACEMENT, not a reparse: the parameter expansion's
// byte range is replaced with the value, and every other byte of src stays exactly
// where the caller put it. That keeps the zero-rewrite guarantee the rest of this
// package rests on — the payload is still a slice of the source the caller wrote,
// only with the named holes filled in.
//
// AN UNUSED VALUE IS REFUSED, and that is a change of policy. It used to be allowed
// on the grounds that cmd and the table are two independent inputs and a caller may
// share one table across several calls — but the caller cannot see the difference:
// values it passed and never referenced are dropped in silence, so "I sent three and
// one arrived" and "all three arrived" produce the same reply. The other direction was
// already an error (`vars does not define $X`), and a rule that only fires one way is
// how a table that is wrong by one entry stays wrong.
//
// The cost is that a shared table has to be trimmed per call. That is the cheaper
// mistake: a caller that notices an unused value learns which one it never referenced,
// and nothing has run yet when it is told.
func ParseWithSubst(src string, sub Subst) (*File, error) {
	if strings.TrimSpace(src) == "" {
		if names := unusedNames(sub, usage{}); len(names) > 0 {
			return nil, &Error{Msg: unusedMessage(sub, names, true)}
		}
		return &File{Empty: true}, nil
	}
	// RAW IS A PROPERTY OF THE PARSE, SO IT NEEDS A FIRST PASS. Which `$N` sits
	// inside a payload cannot be known until the statement is parsed — the payload
	// boundary is what decides — so the first pass is run against the caller's own
	// text only to find those regions, and its failures are not reported: the real
	// parse below is the one that answers.
	//
	// ONE WALK FOR BOTH. The spans (which regions are shell syntax, arithmetic, or
	// here-doc) and the refs (where the `$N` parameter expansions sit and whether an
	// enclosing double-quote owns them) are collected in a single walk over one AST.
	// Running two parses over the same source text only introduced the possibility of
	// them seeing different things.
	raw, refs := analyzeSource(src)
	substituted, sentinels, used, err := substitute(src, sub, raw, refs)
	if err != nil {
		return nil, &Error{Msg: "cannot parse cmd: " + err.Error()}
	}
	if names := unusedNames(sub, used); len(names) > 0 {
		return nil, &Error{Msg: unusedMessage(sub, names, false)}
	}
	src = substituted
	parser := syntax.NewParser()
	file, err := parser.Parse(strings.NewReader(src), "")
	if err != nil {
		return nil, &Error{Msg: "cannot parse cmd: " + err.Error()}
	}
	// THE FIRST PASS'S OFFSETS ARE NOT REUSED: a raw value is a sentinel of a
	// different length, so they no longer address the same bytes. Nothing after the
	// parse needs them — the payload span is known at parse time, which is exactly
	// what made the raw decision possible in the first place.
	out := &File{}
	for _, stmt := range file.Stmts {
		if stmt == nil || stmt.Cmd == nil {
			continue
		}
		// A `&&`/`||` CHAIN IS SEVERAL DIRECTIVES, NOT ONE PAYLOAD.
		//
		// The shell parser reports `a && b` as a single Stmt holding a BinaryCmd,
		// which is right for shell text and wrong for a directive line: the shell
		// operators were being swallowed into run's payload, so a caller writing
		// `run h:1 make && notify done` got one shell command line instead of two
		// directives with a condition between them.
		//
		// Shell text still arrives intact — through `$1`/`$FOO`, which are replaced
		// with opaque tokens BEFORE this parse, so a `&&` inside a value is part of
		// that token and cannot appear here as an operator.
		for _, part := range flattenCond(stmt) {
			parsed, ok, err := parseStatement(src, part.stmt, sentinels)
			if err != nil {
				// The first failure wins: a cmd is one instruction, and reporting the
				// rest of a broken instruction teaches nothing. A directive the caller
				// defined itself (an alias) surfaces as an unknown-directive error
				// carrying Op+Rest, which is what lets the caller resolve it.
				if derr, ok := err.(*Error); ok {
					derr.Parsed = len(out.Statements)
					derr.AfterPayload = trailingPayloadOp(out.Statements)
				}
				return nil, err
			}
			if !ok {
				continue
			}
			// The statement's own punctuation, before anything asks about its shape:
			// a trailing `&` is this language's operator and the payload check has to
			// know that, or `run h:1 $1 &` looks like a payload with an `&` in it.
			parsed.Background = part.stmt.Background

			// WHAT THE PAYLOAD IS, BEFORE IT STOPS BEING A REFERENCE.
			if op, known := Lookup(parsed.Op); known {
				if err := checkPayloadType(parsed, sentinels, op); err != nil {
					if derr, ok := err.(*Error); ok {
						derr.Parsed = len(out.Statements)
					}
					return nil, err
				}
			}
			filled := sentinels.expandStatement(parsed)
			// WHAT THE VALUE IS, AFTER IT IS THE VALUE. The whole-word rule and the
			// payload rule have to run BEFORE this line, because both are questions
			// about the shape of the caller's text and a hole is still visible there.
			// A domain question is the opposite: `--repeat $1` is a number when the
			// value arrives, and asking before the holes are filled refused it as
			// "got \"__hole_…\"" — every numeric, enum and geometry flag was
			// unreachable through a reference, and the caller was told their 3 was not
			// a number.
			if op, known := Lookup(filled.Op); known {
				if err := checkParamDomains(filled, op); err != nil {
					if derr, ok := err.(*Error); ok {
						derr.Parsed = len(out.Statements)
					}
					return nil, err
				}
			}
			if err := checkTargetKindAfterSubst(filled); err != nil {
				if derr, ok := err.(*Error); ok {
					derr.Parsed = len(out.Statements)
				}
				return nil, err
			}
			// The first statement of the whole cmd has nothing to be conditional on,
			// and a `;`-separated statement starts a fresh chain: flattenCond reports
			// "" for those, so the executor never waits on a result that is not there.
			if len(out.Statements) == 0 {
				filled.Cond = ""
			} else {
				filled.Cond = part.cond
			}
			filled.Background = part.stmt.Background
			out.Statements = append(out.Statements, filled)
		}
	}
	if len(out.Statements) == 0 {
		return &File{Empty: true}, nil
	}
	return out, nil
}

// condPart is one directive in a `&&`/`||` chain: the statement to parse and the
// operator that connects it to the one before it.
type condPart struct {
	stmt *syntax.Stmt
	cond string // "" (unconditional), "&&", "||"
}

// flattenCond splits a shell BinaryCmd into the directives it connects.
//
// `a && b || c` parses as BinaryCmd(||, BinaryCmd(&&, a, b), c), so the left spine
// is the chain in source order: walk X while it is a BinaryCmd with an operator this
// language gives meaning to, then emit the leaves.
//
// ONLY `&&` AND `||` ARE SPLIT. `|` is a pipeline, and a pipeline is one command
// line whose parts run concurrently — there is no "did the left succeed" to gate on,
// so it stays inside the payload. `&` and `;` never reach here: the shell parser
// already made them separate Stmts, which is what makes `;` start a fresh chain.
func flattenCond(stmt *syntax.Stmt) []condPart {
	var parts []condPart
	var walk func(s *syntax.Stmt, cond string)
	walk = func(s *syntax.Stmt, cond string) {
		if b, ok := s.Cmd.(*syntax.BinaryCmd); ok {
			switch b.Op {
			case syntax.AndStmt, syntax.OrStmt:
				walk(b.X, cond)
				op := "&&"
				if b.Op == syntax.OrStmt {
					op = "||"
				}
				walk(b.Y, op)
				return
			}
		}
		parts = append(parts, condPart{stmt: s, cond: cond})
	}
	walk(stmt, "")
	return parts
}

// trailingPayloadOp names the last statement's directive when it was one that
// hands raw bytes to a terminal, or "" when it was not.
//
// It answers one question for the reply: was the caller writing SHELL text when
// the parse broke, or termcp instructions? `run h:1 pwd; whoami` is the first case,
// and the `;` the shell would have accepted is the thing that ended the payload. The
// reply can say so, but only if the parser leaves a mark here — the failing
// statement is a bare word (`whoami`) and carries no trace of what preceded it.
func trailingPayloadOp(stmts []Statement) string {
	if len(stmts) == 0 {
		return ""
	}
	n := stmts[len(stmts)-1].Op
	op, known := Lookup(n)
	if !known {
		return ""
	}
	if op.hasPayload() {
		return op.Name
	}
	return ""
}

// parserRefs lists every reference the grammar reports, plus the ones it cannot report.
//
// THE GRAMMAR IS ASKED, NOT IMITATED. A parameter expansion is a node; whether it sits
// inside double quotes is a question about the nodes it sits inside; whether it is
// positional is a field of the node. All three were previously re-derived by scanning
// bytes and toggling a flag, which is a second opinion about a fact the parser had already
// established — and every disagreement between the two was a defect (issue #141 among
// them).
//
// THE ONE THING THE GRAMMAR CANNOT ANSWER is a `$` inside a single-quoted string: that
// node's text is a single field with no children, because a shell expands nothing there.
// The parser is right about shell, so in a shell region that is the end of it — a quoted
// reference is the shell's literal. In a region no shell reads (a frame, `--data`, a name)
// the bytes are the truth instead, and `resolve` reads them there.
// analyzeSource runs the first AST pass over the caller's raw text.
//
// It answers two questions at once from ONE parsed tree:
//   - which byte ranges hold shell syntax, arithmetic expressions, or here-docs (spans);
//   - which parameter expansions exist and whether a double-quote encloses them (refs).
//
// Doing both in one walk guarantees they share the exact same view of the line:
// an arithmetic node cannot be in one tree and its `$1` in another. A source that
// does not parse yields nil for both; the real parse in ParseWithSubst then reports
// the syntax error where it belongs.
func analyzeSource(src string) ([]span, []ref) {
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		return nil, nil
	}
	var (
		dq    [][2]int
		refs  []ref
		spans []span
	)
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.DblQuoted:
			// Pre-order: enclosing quote is recorded before nodes inside it.
			dq = append(dq, [2]int{int(n.Pos().Offset()), int(n.End().Offset())})
		case *syntax.ParamExp:
			if r, ok := refOfParam(src, n); ok {
				r.dq = insideRange(dq, r.start, r.end)
				refs = append(refs, r)
			}
		}
		// TWO REGION KINDS USED TO BE COLLECTED HERE, and both are unreachable now.
		//
		// The rule that a reference is the WHOLE word it sits in (see
		// checkOneWordOneLanguage) means no reference can sit inside shell syntax any
		// more, so a region describing shell syntax has nothing left to decide:
		//
		//   - ARITHMETIC (`ArithmExp`): `--data $(( $1 + 1 ))` and `run h:1 echo $(( $1 +
		//     1 ))` are both refused — the reference is glued to the operators around it.
		//     Every statement that produced one of these spans is refused, so the span
		//     never reached a decision: measured, the insertion mode it selects
		//     (`spanArithmetic`) does not appear in a single accepted statement.
		//   - A HERE-DOCUMENT BODY (`Redirect.Hdoc`): a here-doc needs a redirect, and a
		//     redirect is refused on a verb that runs no command line, while a payload
		//     verb's payload must be exactly one reference — `run h:1 $1 <<EOF` is
		//     refused for that reason. The body's references are refused with it.
		//
		// ponytail: this is deleted BECAUSE the whole-word rule holds. Lifting that rule
		// is the one change that makes both regions live again, and this comment is
		// where the two walks go back.
		return true
	})
	for _, stmt := range file.Stmts {
		if stmt == nil || stmt.Cmd == nil {
			continue
		}
		// ONE SPAN PER DIRECTIVE, NOT PER SHELL STATEMENT. `a && b` is one shell
		// statement holding two directives, so walking file.Stmts alone would leave
		// the second operand without a span.
		for _, part := range flattenCond(stmt) {
			spans = append(spans, statementRawSpans(src, part.stmt)...)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].start < refs[j].start })
	return spans, refs
}

// refOfParam reads one expansion, or declines it.
//
// Declining is the point of the list: `${x:-y}`, `${#x}`, `${x/a/b}`, `$@`, `$?` and `$$`
// are the SHELL's constructs, and this protocol does not rewrite a construct whose meaning
// depends on a variable it never sees.
func refOfParam(src string, p *syntax.ParamExp) (ref, bool) {
	if p.Excl || p.Length || p.Width || p.Index != nil || p.Slice != nil || p.Repl != nil || p.Exp != nil {
		return ref{}, false
	}
	name := p.Param.Value
	if name != "" && !isIdent(name) && !allDigits(name) {
		return ref{}, false
	}
	// `Short` is the unbraced spelling (`$N`), NOT "positional": `$N` is a name and `$1`
	// is an index, and the two live in different namespaces. Only digits are positional.
	return ref{
		start:      int(p.Pos().Offset()),
		end:        int(p.End().Offset()),
		name:       name,
		positional: allDigits(name),
	}, true
}

// insideRange reports whether [start,end) sits inside any of these ranges.
func insideRange(ranges [][2]int, start, end int) bool {
	for _, r := range ranges {
		if start >= r[0] && end <= r[1] {
			return true
		}
	}
	return false
}

// statementRawSpans reports the ranges of a statement whose values are SHELL
// SYNTAX rather than data. There are two, and both are read as a command line by
// something downstream:
//
//   - the payload of run/type, which goes straight to the terminal;
//   - the value of a RestValue flag (--argv on pipe/pty, --args on ext), which is
//     handed to SplitArgv and parsed as a word list.
//
// The second one is why this returns a list, and why quoting a value for --argv is
// wrong in a way that is easy to miss: the flag's value is sliced from the SOURCE
// and then RE-PARSED, so a quoted `sh -c 'echo hi'` arrives as one word and the
// server tries to exec a program with that name. A raw splice is what makes an argv
// an argv.
//
// IT MIRRORS parseStatement'S THREE KINDS, because a span that disagrees with the
// parser about where a payload starts is worse than no span at all: the value would
// be spliced into the wrong bytes.
func statementRawSpans(src string, stmt *syntax.Stmt) []span {
	call := leadCall(stmt.Cmd)
	if call == nil || len(call.Args) == 0 {
		return nil
	}
	name, ok := literalWord(src, call.Args[0])
	if !ok {
		return nil
	}
	op, known := Lookup(name)
	if !known {
		return nil
	}
	bodyEnd := commandEnd(stmt)
	rest := call.Args[1:]
	// The target is consumed exactly where the parser consumes it, and it is RAW
	// when it is there.
	//
	// A target is an IDENTIFIER (`<session>:<n>`, a profile name, an extension id),
	// not a shell word: quoting it produced `info 's:1'`, which happens to still
	// resolve because the resolver trims quotes — but the Path echo then showed a
	// spelling nobody wrote, and any directive that compared the target as a string
	// would have seen the quotes as part of the name.
	var out []span
	if op.Target && len(rest) > 0 && !strings.HasPrefix(mustLiteralWord(src, rest[0]), "--") {
		out = append(out, span{
			start: int(rest[0].Pos().Offset()),
			end:   int(rest[0].End().Offset()),
			mode:  spanVerbatim,
		})
		rest = rest[1:]
	}

	if op.Kind == KindFlags {
		// A flag-kind directive takes flags ANYWHERE, with positional words in
		// between (`ext <session> --args …` is a positional and then a flag). Only a
		// RestValue flag contributes a span, and it swallows the rest of the line.
		for i := 0; i < len(rest); i++ {
			w := mustLiteralWord(src, rest[i])
			if !strings.HasPrefix(w, "--") {
				continue
			}
			key, _ := splitFlag(w)
			if !allowedFlag(op, key) {
				return out
			}
			if restValueFlag(op, key) && i+1 < len(rest) {
				// PER-WORD: `--args` on ext is parsed again by SplitArgv, so each
				// whole-word `$N` is one argv element and must arrive unquoted.
				return append(out, span{start: int(rest[i+1].Pos().Offset()), end: bodyEnd, mode: spanWords})
			}
			if !strings.Contains(w, "=") && !valuelessFlag(op, key) && i+1 < len(rest) {
				i++ // the flag's value word
			}
		}
		return out
	}

	// The payload kinds: leading flags, then a payload that runs to the end of the
	// statement. A flag AFTER the payload's first word belongs to the payload, which
	// is the same rule the parser applies.
	i := 0
	for i < len(rest) && strings.HasPrefix(mustLiteralWord(src, rest[i]), "--") {
		w := mustLiteralWord(src, rest[i])
		key, _ := splitFlag(w)
		if !allowedFlag(op, key) {
			return out
		}
		if restValueFlag(op, key) {
			// pipe/pty take their program this way, and the value is re-parsed by
			// SplitArgv, so it is syntax like a payload is. PER-WORD, for the same
			// reason as the flag kind above: each whole-word `$N` is one argv element
			// and must arrive unquoted.
			if i+1 < len(rest) {
				return append(out, span{start: int(rest[i+1].Pos().Offset()), end: bodyEnd, mode: spanWords})
			}
			return out
		}
		i++
		if !strings.Contains(w, "=") && !valuelessFlag(op, key) && i < len(rest) {
			i++
		}
	}
	if i >= len(rest) || !op.hasPayload() {
		return out
	}
	return append(out, span{
		start: int(rest[i].Pos().Offset()),
		// THE REGION IS THE COMMAND LINE; THE PAYLOAD IS ITS WORDS. A redirect or a pipe
		// after the words is still read by the shell, so a reference there is shell syntax
		// and gets quoted as data — while `$1 | wc -l` has `$1` as its whole payload and
		// hands the terminal a command line.
		end:        bodyEnd,
		payloadEnd: wordsEnd(rest),
		// THE END COMES FROM THE SAME WORD LIST AS THE START (issues #141, #155).
		//
		// It used to be `commandEnd(stmt)` — the end of the shell STATEMENT — and a
		// shell statement is not a directive. `a && b` is ONE statement holding TWO
		// directives, and `a | b` is one statement whose payload is only the leading
		// command, so the region ran past what the directive calls its payload. The
		// substitution rule then asks "is the reference the whole region" and answers
		// no, which turns a command line into a single quoted word:
		//
		//	run h:1 $1 &              region "$1 "    terminal got `'echo C' &`
		//	run h:1 $1 ; ls host      region "$1 "    terminal got `'echo D' `
		//	run h:1 $1 && run h:1 $2  region "$1 && …" and `$2` had no region at all
		//
		// Taking both ends from the AST word list makes the region exactly the
		// payload, so the exact-equality test in the substitution is right again and
		// needs no heuristic about trailing punctuation.
		//
		// `bodyEnd` still bounds the payload TEXT that goes to the terminal: that is
		// the whole line, pipes and redirects included. Only the REGION — the answer
		// to "is this reference the payload" — stops at the last payload word.
		mode: op.payloadSpanMode(),
	})
}

// wordsEnd is the source offset just past the last of these words.
//
// Empty means there is no payload to bound, which callers check before asking.
func wordsEnd(rest []*syntax.Word) int {
	if len(rest) == 0 {
		return 0
	}
	return int(rest[len(rest)-1].End().Offset())
}

// mustLiteralWord is literalWord for the span pass, which is allowed to give up.
func mustLiteralWord(src string, w *syntax.Word) string {
	s, _ := literalWord(src, w)
	return s
}

// shellSingleQuote renders a value as a single-quoted shell word.
func shellSingleQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// shellDoubleQuoteBody renders a value for insertion INSIDE existing double
// quotes: backslash, double quote and dollar must be escaped, and a literal
// newline is left as-is (it is data inside quotes).
func shellDoubleQuoteBody(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")
	return r.Replace(v)
}

// isSentinel reports whether a value is still a raw hole waiting to be filled.
//
// It matches the MARKER, not the whole token: the rest of a hole's name is random per
// call (sentinelPrefix), so no reader can know the exact string in advance. A hole that
// reaches a reader is a bug either way, and the marker is what makes one recognizable —
// which is also why a leaked hole is now an obvious impossibility rather than a legal-
// looking path.
func isSentinel(v string) bool {
	return strings.Contains(v, sentinelMarker)
}

// isWholeReference reports whether a word is EXACTLY one reference and nothing else.
//
// isSentinel asks a different question — "does this text contain a reference" — and
// the difference is the whole point here: `{"v":"$1"}` is a FRAME whose template text
// happens to hold a reference, so it is a PAYLOAD and the peer expands it. Asking
// isSentinel would read that frame as a bare reference and refuse it as grammar.
func isWholeReference(v string) bool {
	return isSentinel(strings.Trim(v, `'"`))
}

// checkTargetKindAfterSubst re-runs the target-kind rule on a statement whose
// sentinels have been filled in.
//
// The rule belongs at PARSE time — refusing a bad target with a usage line before
// anything runs is the whole point — but a substituted target has no value yet when
// the parser sees it. Running the check in both places keeps the early refusal for a
// literal target and closes the hole for one that came from args.
func checkTargetKindAfterSubst(st Statement) error {
	op, known := Lookup(st.Op)
	if !known || !op.Target || st.Target == "" {
		return nil
	}
	if err := checkTargetKind(op, st.Target); err != nil {
		return err
	}
	return nil
}

// leadCall finds the first CallExpr of a statement, in source order.
//
// A statement can be a pipeline or a chain (`a | b`, `a && b`); the directive is
// whatever the FIRST command is, and everything from its payload onwards belongs
// to that directive. Walking down the left spine finds it without depending on
// the statement's shape.
func leadCall(cmd syntax.Command) *syntax.CallExpr {
	switch c := cmd.(type) {
	case *syntax.CallExpr:
		return c
	case *syntax.BinaryCmd:
		if left := leadCall(c.X.Cmd); left != nil {
			return left
		}
		return leadCall(c.Y.Cmd)
	case *syntax.IfClause:
		if len(c.Then) > 0 {
			return leadCall(c.Then[0].Cmd)
		}
	case *syntax.Block:
		if len(c.Stmts) > 0 {
			return leadCall(c.Stmts[0].Cmd)
		}
	}
	return nil
}

// commandEnd is the source offset where a statement's command text ends,
// excluding the `;` separator that stmt.End() counts as part of the statement.
//
// A payload is sliced to this offset so the caller's sequencing punctuation stays
// out of the bytes sent to the shell: `run h echo hi;` must send `echo hi`, not
// `echo hi;`. Everything else about the statement is unchanged — a pipeline or an
// `&&` chain is still one command, and its text still ends at its last word.
func commandEnd(stmt *syntax.Stmt) int {
	if stmt.Semicolon.IsValid() {
		// A BACKGROUNDING `&` IS PART OF THE LINE, and the AST says where it is:
		// `Stmt.Semicolon` is documented as "position of ';', '&', or '|&'".
		//
		// Trimming it the way a `;` is trimmed silently changed what the caller asked
		// for: `run h:1 sleep 100 &` sent `sleep 100 `, so the shell ran it in the
		// FOREGROUND and the call blocked until it finished — a 200 that did the
		// opposite of the line as written. The `;` is different in kind: it ends the
		// termcp statement, so it is termcp's punctuation and never the shell's.
		if stmt.Background {
			return int(stmt.Semicolon.Offset()) + 1
		}
		return int(stmt.Semicolon.Offset())
	}
	return int(stmt.End().Offset())
}

// checkValueWasNotCutOff refuses the two shapes where a flag's value silently ENDS.
//
// THIS IS THE SAME DEFECT THE PAYLOAD RULE FIXES, one layer down, and it is why this
// language kept leaking into itself. A directive's words are read by two grammars at
// once — this one and the shell's — and wherever the shell grammar takes over without
// saying so, the caller's bytes stop meaning what they wrote. Measured, not imagined:
//
//	write s file /x --data a>b      the shell reads `>b` as a REDIRECT, so the value
//	                                became "a", `>b` went nowhere, and the reply was
//	                                200 with no error anywhere.
//	write s file /x --data a b      `b` is a second word: the value is "a" and `b`
//	                                becomes a positional that nothing reads.
//
// The fix is not to interpret those bytes — that is the shell's job, and this parser
// does not run commands. It is to REFUSE them and say which value to pass instead. A
// value the grammar cannot cut is the only kind that means what it says.
func checkValueWasNotCutOff(op Op, flag, value string, rest []word, at int, holes *sentinelTable) error {
	// A WORD RIGHT AFTER THE VALUE IS A WORD THAT WAS CUT OFF. This is checked at the
	// one place that knows a value flag just took a word, rather than by guessing from
	// arity: `key s:1 enter` and `help read` take bare words legitimately, and a rule
	// about "leftover words" would refuse them.
	//
	// A PAYLOAD VERB IS EXEMPT: the word after its flags IS the payload (`notify --for
	// 5s the body`), and that payload has its own declaration and its own check.
	if op.hasPayload() {
		return nil
	}
	if at+1 < len(rest) && !strings.HasPrefix(rest[at+1].text, "--") {
		return &Error{
			Op: op.Name,
			Msg: "--" + flag + " took " + strconv.Quote(holes.spell(value)) + " as its value, and " + strconv.Quote(holes.spell(rest[at+1].text)) +
				" is a separate word after it — so it is not part of the value and nothing would use it.\n" +
				"Quote a value that contains a space (--" + flag + " \"a b\"), or pass it as args.",
			Usage: op.Usage,
		}
	}
	return nil
}

// checkNoRedirect refuses a redirect on a verb that does not run a command line.
//
// A REDIRECT IS THE SHELL'S, and this parser does not run commands: `write … --data
// a>b` was accepted with the value "a" and the `>b` dropped on the floor. The payload
// directives are exempt because their payload IS the rest of the line — `call` sends a
// frame verbatim and `run`/`input` take a reference, where a redirect is not reachable
// through a spelling this table documents.
func checkNoRedirect(stmt *syntax.Stmt, op Op) error {
	if len(stmt.Redirs) == 0 || op.hasPayload() {
		return nil
	}
	r := stmt.Redirs[0]
	return &Error{
		Op: op.Name,
		Msg: op.Name + " does not take a redirect, and " + strconv.Quote(r.Op.String()) + " was read as one by the " +
			"shell grammar — so everything after it was cut off the value you wrote.\n" +
			"Shell operators belong inside a VALUE: quote it (--data \"a>b\"). A command line belongs in args.",
		Usage: op.Usage,
	}
}

// checkOneWordOneLanguage enforces the rule the whole language rests on: cmd IS THE DSL.
//
// A reference is either the WHOLE WORD it sits in, or it is not in cmd at all. There is
// no spelling in which a value and a literal become one word, because a word made of two
// languages is read twice and means something different to each reader:
//
//	write h file /tmp/$1              a path assembled from a literal and a value
//	--data a$1                        the value "a" glued to the caller's bytes
//	--data $(( $1 + 1 ))              a value that is also an arithmetic expression
//
// THIS IS CHECKED ON THE WORD LIST, not on the statement's fields. Every field below
// (target, kind key, positional, flag value, word list) is cut from these words, so one
// check here covers all of them — and a field-by-field version would have to be
// remembered again for each field added later.
//
// THE PAYLOAD IS NOT HERE, and does not need to be: it is sliced from the raw source
// rather than built from words, and checkPayloadType already requires it to be exactly
// one reference. A frame (`call`, PayloadSpan spanVerbatim) is exempt by declaration:
// it never passes through a shell, so a reference inside its template is the peer's.
func checkOneWordOneLanguage(op Op, rest []word, holes *sentinelTable) error {
	if holes == nil || holes.prefix == "" {
		// No substitution ran, so there is nothing to be glued to: every `$N` in cmd is
		// the shell's own text and this language has no claim on it.
		return nil
	}
	for _, w := range rest {
		if !strings.Contains(w.text, holes.prefix) || holes.isOneWordHole(w.text) {
			continue
		}
		return &Error{
			Op: op.Name,
			Msg: "cmd mixes a value with shell text: " + strconv.Quote(holes.spell(w.text)) + "\n" +
				"cmd IS THE DIRECTIVE LANGUAGE. It holds verbs, addresses and references — a reference " +
				"($1, $2, $NAME) has to be the WHOLE word it sits in, never glued to a literal, because a " +
				"word made of two languages is read twice and means something different to each reader. " +
				"There is no shell concatenation here.\n" +
				"Pass the whole thing as the value instead:\n" +
				"  cmd: \"" + op.Name + " … $1\"   args: [<the whole value>]",
			Usage: op.Usage,
		}
	}
	return nil
}

// checkPayloadType enforces what an Op declared its payload to BE.
//
// IT IS CHECKED BEFORE THE HOLES ARE FILLED IN, which is the only moment the question
// has an answer: afterwards `$1` is gone and the payload is the shell line itself, so
// "was this a value or a line?" can no longer be asked. The payload at this point is a
// slice of the SUBSTITUTED source, so a reference appears as its stand-in token — one
// token, and nothing else around it, is exactly what "the payload is one variable"
// means in that text.
//
// WHY `run` NEEDS IT. The payload of `run` used to be "the rest of the line, read by
// the shell parser at the same time as the directive is read by this one". That is two
// languages sharing one text, and every combination of them is a different guess:
//
//	run h:1 ls | write h:2 file /x   a shell pipeline into a program called `write`
//	run h:1 echo a; b                two termcp directives
//	run h:1 make && notify done      a condition, or a shell `&&`?
//
// Requiring one variable reference removes the guess without removing the capability:
// the shell text still arrives intact, it just arrives as a VALUE — `args: ["ls | wc
// -l"]` is the command line, and the payload is the reference that carries it.
// referenceIsGrammar refuses a reference that reached a word the GRAMMAR decides.
//
// THIS IS THE DSL ESCALATION BOUNDARY, and it is deliberately a refusal rather than
// a smarter parse. A reference is filled in after the statement is parsed, so by the
// time the value arrives the shape is already fixed: a word meant to be a kind, a flag
// name, a key name or a channel index cannot become one, and it falls through to a
// positional argument — where the verb may read it as a different operation, or not
// read it at all. `close <session> $1` closed the WHOLE SESSION instead of channel 2
// that way, and answered 204 (issue #199).
//
// The alternative is not a smaller refusal but a different language: substituting
// first and parsing after would let a caller's DATA choose which directive runs. That
// is the escalation, so the reference stops here and the caller is told what to write.
func referenceIsGrammar(op Op, where string, options ...string) error {
	var b strings.Builder
	b.WriteString("a reference fills a VALUE; it never becomes part of the grammar.\n")
	b.WriteString("THIS ONE SITS " + where + ". The word cannot be read until the value arrives,\n")
	b.WriteString("and the statement's shape is decided before that — so it would fall through to a\n")
	b.WriteString("positional word, and the command would mean something else. Refused, not reinterpreted.\n")
	b.WriteString("Here is how to say it instead:\n")
	for _, o := range options {
		b.WriteString("  " + o + "\n")
	}
	b.WriteString("the same value is always reachable as data — only the spelling changes.")
	return &Error{Op: op.Name, Msg: b.String(), Usage: op.Usage}
}

func checkPayloadType(st Statement, holes *sentinelTable, op Op) error {
	if op.PayloadType != PayloadVar {
		return nil
	}
	raw := strings.TrimSpace(st.Payload)
	// A TRAILING `&` IS TERMCP'S OPERATOR, NOT THE SHELL'S. `commandEnd` keeps it in the
	// payload on purpose — for a literal shell line `sleep 100 &` it is the shell's
	// backgrounding — but a var payload cannot carry shell punctuation any more, so the
	// only thing a trailing `&` can mean is "run this statement concurrently". A shell
	// level `&` goes in the value: args ["sleep 100 &"].
	if st.Background {
		raw = strings.TrimSpace(strings.TrimSuffix(raw, "&"))
	}
	// TWO SPELLINGS ARE THE SAME ANSWER. A substituted source holds this call's
	// stand-in token where the reference was; an un-substituted one (`Parse`, and the
	// usage lines a caller reads) still holds `$1`. Both mean "one variable", and the
	// rule is about the SHAPE of the payload, not about which pass is looking at it.
	if holes.isOneHole(raw) || isSingleRef(raw) {
		return nil
	}
	return &Error{
		Op: op.Name,
		Msg: op.Name + " takes a VARIABLE, not a command line: its payload must be exactly one reference " +
			"($1, $2 … or $NAME), and the shell text goes in that value.\n" +
			"Writing the line directly is refused because the payload would then be read by two languages " +
			"at once: `" + firstLine(holes.spell(raw)) + "` is a shell pipeline to one reader and a termcp directive to " +
			"the other, and neither reading is a guess worth making.\n" +
			"Use: " + op.Name + " <channel> $1   with args: [\"the command line\"]",
		Usage: op.Usage,
	}
}

// isSingleRef reports whether s is exactly one variable reference and nothing else:
// `$1`, `${12}`, `$NAME`.
func isSingleRef(s string) bool {
	if len(s) < 2 || s[0] != '$' {
		return false
	}
	rest := s[1:]
	if rest[0] == '{' {
		return len(rest) > 2 && rest[len(rest)-1] == '}' && strings.Count(rest, "}") == 1
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return false
	}
	return true
}

// firstLine keeps a refusal short enough to read.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// parsePipeline reads `a | b` as two statements, and refuses every pairing that is
// not the one pipeline this language has.
//
// THE REFUSAL IS THE FEATURE. `|` inside a payload is the REMOTE SHELL's operator
// (`run h:1 $1 | wc -l`), so a caller that writes `run h:1 ls | write h:2 file /x`
// is not asking for a copy — it is asking the remote shell to pipe into a program
// called `write`, and on Linux that program exists (it messages another user's
// terminal). The two meanings cannot be told apart from the character, only from the
// verb, so the verb is what this checks and what the error names.
func parsePipeline(src string, outer *syntax.Stmt, bin *syntax.BinaryCmd, holes *sentinelTable) (Statement, bool, error) {
	left, ok, err := parseStatement(src, bin.X, holes)
	if err != nil {
		return Statement{}, false, err
	}
	if !ok {
		return Statement{}, false, nil
	}
	// THE RIGHT SIDE IS NAMED BEFORE IT IS PARSED. `read a1f3:1 | grep x` is the
	// mistake this whole check exists for — a caller writing a SHELL pipeline — and
	// the generic "unknown directive: grep" answers a question they did not ask.
	// Naming the rule is the answer to the one they did.
	if call := leadCall(bin.Y.Cmd); call != nil && len(call.Args) > 0 {
		if word, ok := literalWord(src, call.Args[0]); ok {
			if _, known := Lookup(word); !known {
				return Statement{}, false, &Error{
					Op: left.Op,
					Msg: "`" + word + "` is not a directive, and the right side of a termcp `|` must be one: " +
						"`read … | write …` copies bytes from one node to another, and it is the only pipeline " +
						"in this language.\nA SHELL pipeline is written inside a payload, where `|` belongs to " +
						"the shell: `run h:1 $1` with args [\"ls | " + word + "\"].",
					Usage: "read <session> <index> | write <session> file <path>",
				}
			}
		}
	}
	right, ok, err := parseStatement(src, bin.Y, holes)
	if err != nil {
		return Statement{}, false, err
	}
	if !ok {
		return Statement{}, false, nil
	}

	// Background and Cond are NOT set here: the caller of parseStatement reads them
	// off the outer statement, which is the one that carries the `&` or the `&&`.
	// `read … | write … &` therefore stays a backgrounded copy.
	refuse := func(msg string) (Statement, bool, error) {
		return Statement{}, false, &Error{Op: left.Op, Msg: msg,
			Usage: "read <session> <index> [--stream o|e] | write <session> file <path>"}
	}

	switch {
	case bin.Op == syntax.PipeAll:
		return refuse("`|&` is not a termcp operator: a pipeline moves BYTES from one node to another, " +
			"and each side has exactly one stream. Use `|`, and pick the left side's stream with " +
			"`read … --stream o|e`.")
	case left.Pipe != nil || right.Pipe != nil || right.Piped:
		return refuse("a pipeline here has exactly two ends: `read … | write …`. " +
			"Chaining a third is refused rather than run in an order you would have to guess — " +
			"write the second copy as its own statement.")
	case left.Op != "read" || right.Op != "write":
		return refuse("`|` connects two termcp directives only as `read … | write …`, which copies bytes " +
			"from one node to another without them passing through your context; got `" + left.Op +
			" | " + right.Op + "`.\n" +
			"Inside a payload `|` belongs to the REMOTE SHELL — `run h:1 $1` with args [\"ls | wc -l\"] is " +
			"how a shell pipeline is written. To sequence two directives, use `;` or `&&`.")
	}

	left.Pipe = &right
	right.Piped = true
	// The echo in the reply is the line the caller wrote, not the half this statement
	// happens to be: a receipt for `read … | write …` that echoes only `read …` reads
	// like the copy was never asked for.
	left.Path = strings.TrimSpace(src[outer.Pos().Offset():commandEnd(outer)])
	return left, true, nil
}

// parseStatement reads one statement. ok is false for a statement that is not a
// directive at all (an assignment, a redirection-only command) — those are left
// for the next statement rather than reported, because `cmd` is a shell string
// and a caller may legitimately begin with something else.
func parseStatement(src string, stmt *syntax.Stmt, holes *sentinelTable) (Statement, bool, error) {
	call := leadCall(stmt.Cmd)
	if call == nil || len(call.Args) == 0 {
		return Statement{}, false, nil
	}
	opWord := call.Args[0]
	name, ok := literalWord(src, opWord)
	if !ok {
		// e.g. "$X" as the directive: not a directive this parser can name.
		return Statement{}, false, nil
	}
	op, known := Lookup(name)
	if !known {
		// A caller that typed a name this build does not have is exactly who a
		// usage line helps, so the error carries the whole table rather than
		// making them ask for it in a second call.
		return Statement{}, false, &Error{
			Op:    name,
			Msg:   "unknown directive: " + name,
			Usage: "help",
			Rest:  restOfStatement(src, stmt),
		}
	}

	// A REDIRECT ON A VERB THAT RUNS NOTHING IS A CUT-OFF VALUE, not a shell command.
	if err := checkNoRedirect(stmt, op); err != nil {
		return Statement{}, false, err
	}

	// A PIPELINE IS TWO STATEMENTS CONNECTED, and this is the only place that sees
	// the shell AST, so it is read here. `&&`/`||` are split elsewhere (flattenCond),
	// and `;`/`&` never reach here because the shell parser already made them
	// separate Stmts.
	//
	// A PAYLOAD VERB KEEPS THE PIPE. `run h:1 $1 | wc -l` is the remote shell's
	// pipeline and the payload already contains it (`bodyEnd` covers the whole binary
	// command). The same goes for `--argv`, whose whole point is "the rest of the line
	// is the program's words". So the operator reading applies only to verbs that take
	// neither, and those are exactly the ones where a discarded right side is a bug.
	if bin, isPipe := stmt.Cmd.(*syntax.BinaryCmd); isPipe &&
		(bin.Op == syntax.Pipe || bin.Op == syntax.PipeAll) &&
		!op.hasPayload() && len(op.RestValue) == 0 {
		return parsePipeline(src, stmt, bin, holes)
	}

	// The statement's body ends at the command, not at the `;`/newline that
	// separates it from the next one: stmt.End() includes a `;` separator, so a
	// payload or last word sliced to it would carry the caller's `;` into the
	// shell, the program's argv, or an identifier (`mcp tools a1f3:1;` →
	// "a1f3:1;"). An explicit newline is already excluded by stmt.End().
	bodyEnd := commandEnd(stmt)

	out := Statement{
		/* The CANONICAL name, not the word the caller typed: a synonym is a way in,
		   and everything downstream (the dispatch switch, the capability lookup,
		   the reply's op) keys on the canonical one. `extensions list` therefore
		   runs `ext` with no special case anywhere below this line. */
		Op:    op.Name,
		Flags: map[string]*string{},
		Path:  strings.TrimSpace(src[stmt.Pos().Offset():bodyEnd]),
	}

	var rest []word
	for _, arg := range call.Args[1:] {
		text, _ := literalWord(src, arg)
		rest = append(rest, word{
			text:  text,
			lit:   wordLiteral(src, arg),
			start: arg.Pos().Offset(),
			end:   arg.End().Offset(),
		})
	}

	// ONE WORD, ONE LANGUAGE — asked before any word is read, because a word that is
	// two things at once has no single answer to give the readers below.
	//
	// THE TARGET IS ONE WORD TOO, and it is the first one for every verb.
	if len(rest) > 0 {
		if err := checkOneWordOneLanguage(op, rest[:1], holes); err != nil {
			return Statement{}, false, err
		}
	}
	// WHAT IS LEFT IS ALL DSL, unless this verb takes a payload. A payload verb's
	// remaining words are its flags and then the payload itself, and the payload is not
	// a word: it is sliced from the raw source and checked against the declaration
	// (checkPayloadType), which is the only place that knows whether a payload is a
	// reference, a frame or a shell line. Its FLAGS are still words, and they are
	// checked below where the payload begins and the words before it are known.
	if !op.hasPayload() {
		if err := checkOneWordOneLanguage(op, rest, holes); err != nil {
			return Statement{}, false, err
		}
	}

	// A KIND MAY OCCUPY THE TARGET'S POSITION.
	//
	// Everything below a session is addressed `<session> <kind> <key>`, and for the
	// verbs that also work on top-level collections the kind can stand alone:
	// `ls host` has no session, `rm host prod` names the entry itself. So the word in
	// second position is read as a kind when the table declares it, and as a target
	// otherwise — one rule, no verb-specific special cases.
	//
	// The KEY is the single word after the kind, and only when it is not a flag:
	// `open a1f3 forward --local-port 8080` declares no key, while
	// `read a1f3 file /etc/passwd` declares one.
	tookKind := false
	if kind, inline, ok := kindWordWithKey(op, rest); ok {
		out.Kind = kind
		rest = rest[1:]
		tookKind = true
		if inline != "" {
			out.Key = inline
		} else if !op.keyIsThePayload() && len(rest) > 0 && !strings.HasPrefix(rest[0].text, "--") {
			out.Key = rest[0].text
			rest = rest[1:]
		}
	}

	// Word 2 is the target, for the directives that take one. A flag here is
	// refused rather than read as a target: `key --repeat 3 a1f3:1 up` would
	// otherwise silently target "--repeat", and the error has to say why.
	if op.Target && !tookKind {
		switch {
		case len(rest) == 0:
			if !op.TargetOptional {
				return Statement{}, false, &Error{Op: name, Msg: "directive " + name + " needs a target", Usage: op.Usage}
			}
		case strings.HasPrefix(rest[0].text, "--"):
			if !op.TargetOptional {
				return Statement{}, false, &Error{
					Op:    name,
					Msg:   "flags come after the target, not before it: " + name + " " + rest[0].text,
					Usage: op.Usage,
				}
			}
		default:
			out.Target = rest[0].text
			rest = rest[1:]
			// THE KIND CHECK RUNS AGAIN AFTER SUBSTITUTION, not only here.
			//
			// A target that came from `args` is a sentinel at parse time
			// (`__tm…`), and this check reads the value: `read $1` was refused
			// with `read needs a CHANNEL, not "__tm…"`, which is true of the
			// placeholder and false of the handle the caller passed. A literal target
			// is refused here — before anything runs — and a substituted one is
			// checked again once it is filled in.
			if !isSentinel(out.Target) {
				if err := checkTargetKind(op, out.Target); err != nil {
					return Statement{}, false, err
				}
			}
			// `<session> <index>` is the spaced spelling of a channel. Fold it into the
			// handle form here so that one address exists downstream: the colon and the
			// space are two ways to write the same node, not two code paths.
			// EVERY VERB, NOT ONLY TargetChain: `read a1f3 1` and `close a1f3 1` name a
			// channel too, and the spaced spelling has to work wherever the colon does.
			// The fold only fires when the target is not already a handle and the next
			// word is a bare index, so a session followed by a path or a name is
			// untouched.
			if !isSentinel(out.Target) && !NamesAChannel(out.Target) && len(rest) > 0 {
				if idx, ok := channelIndexWord(rest[0].text); ok {
					out.Target = out.Target + ":" + idx
					rest = rest[1:]
				}
			}
			// A REFERENCE CANNOT BE A KIND (issue #199).
			//
			// The word right after the target is where a kind goes, and a reference is
			// filled in AFTER the statement's shape is decided — so the parser cannot see
			// what it will be, cannot match it against op.Kinds, and it falls through to a
			// positional word instead. `close <session> $1` closed the WHOLE SESSION
			// rather than channel 2 that way, and answered 204.
			//
			// The alternative costs the caller nothing, because the WHOLE handle is a
			// value: `close $1` with args ["<session>:2"] says the same thing and is
			// already supported.
			//
			// A PAYLOAD VERB IS THE ONE PLACE A LONE REFERENCE IS STILL A VALUE: `call h $1`
			// hands the frame over whole, and there is no kind in that spelling to be
			// confused with. Two words are the ambiguous shape — `call s $1 $2` can only
			// mean "kind, frame" — and that is refused.
			lonePayload := op.Kind == KindPayload && len(rest) == 1
			if len(op.Kinds) > 0 && len(rest) > 0 && !lonePayload && isWholeReference(rest[0].text) {
				where := "where the channel index goes (it folds into the address as <target>:N)"
				opts := []string{
					"an index   — put it in the address:  " + name + " <target>:2",
					"an address — pass the whole handle as the target:  " + name + " $1   args [\"<target>:2\"]",
				}
				if len(op.Kinds) > 0 {
					where = "where a kind or the channel index goes"
					opts = append([]string{
						"a kind    — write it literally, value beside it:  " + name + " <target> " + op.Kinds[0] + " 1",
					}, opts...)
				}
				return Statement{}, false, referenceIsGrammar(op, where, opts...)
			}
			// A kind may also follow the target: `open a1f3 forward`,
			// `read a1f3 file /x`, `rm a1f3 file /x`.
			if kind, inline, ok := kindWordWithKey(op, rest); ok {
				out.Kind = kind
				rest = rest[1:]
				if inline != "" {
					out.Key = inline
				} else if !op.keyIsThePayload() && len(rest) > 0 && !strings.HasPrefix(rest[0].text, "--") {
					out.Key = rest[0].text
					rest = rest[1:]
				}
			}
		}
	}

	switch op.Kind {
	case KindPayload, KindInterpreted:
		// Leading --name flags belong to the directive (`pipe mac --name x --argv
		// go test`); everything from the first non-flag word to the end of the
		// statement is the payload, raw — except a RestValue flag, whose value is
		// the rest of the statement itself.
		i := 0
		for i < len(rest) && strings.HasPrefix(rest[i].text, "--") {
			key, val := splitFlag(rest[i].text)
			if !allowedFlag(op, key) {
				msg := "unknown flag for " + name + ": --" + key
				if hint, ok := op.RetiredFlags[key]; ok {
					msg += "\n" + hint
				}
				return Statement{}, false, &Error{
					Op:    name,
					Msg:   msg,
					Usage: op.Usage,
				}
			}
			if restValueFlag(op, key) {
				if i+1 >= len(rest) || strings.HasPrefix(rest[i+1].text, "--") {
					return Statement{}, false, &Error{
						Op:    name,
						Msg:   "--" + key + " needs a command, e.g. " + name + " <target> --" + key + " sleep 5",
						Usage: op.Usage,
					}
				}
				// A known flag after --argv is the caller's flag, not the
				// program's argument. Feeding it to the program would be the
				// silent drop this directive exists to avoid.
				for _, w := range rest[i+1:] {
					if k, _ := splitFlag(w.text); strings.HasPrefix(w.text, "--") && allowedFlag(op, k) {
						return Statement{}, false, &Error{
							Op:    name,
							Msg:   "flags come before --" + key + ": --" + k + " would otherwise be passed to the program",
							Usage: op.Usage,
						}
					}
				}
				tail := src[rest[i+1].start:bodyEnd]
				if val != "" {
					val += " " + tail
				} else {
					val = tail
				}
				// A RestValue flag requires a following word, so this branch
				// always HAS a value: it is never absent and never empty.
				storeFlag(out.Flags, key, strings.TrimSpace(val))
				i = len(rest)
				break
			}
			hadEquals := strings.Contains(rest[i].text, "=")
			if val == "" && !hadEquals && !valuelessFlag(op, key) && i+1 < len(rest) && !strings.HasPrefix(rest[i+1].text, "--") {
				val = rest[i+1].lit
				hadEquals = true
				i++
			}
			// Three states, preserved: no `=` and no value word means the flag was
			// written without one (nil); `--name=` and an explicitly empty value
			// both carry an explicit empty string.
			if !hadEquals && !valuelessFlag(op, key) && val == "" {
				out.Flags[key] = nil
			} else {
				storeFlag(out.Flags, key, val)
			}
			// THE VALUE ENDED HERE, AND THIS IS THE ONLY PLACE THAT KNOWS IT. Both
			// spellings are covered because the check is about what FOLLOWS the value:
			// `--data a b` and `--data=a b` are the same truncation.
			if err := checkValueWasNotCutOff(op, key, val, rest, i, holes); err != nil {
				return Statement{}, false, err
			}
			i++
		}
		// THE WORDS IN FRONT OF A PAYLOAD ARE STILL WORDS. The top of this function skips
		// them because a payload verb's remaining words end in the payload, which is not a
		// word; the flags in front of it are cmd, and this is the one point that knows
		// where the payload starts.
		if op.hasPayload() {
			if err := checkOneWordOneLanguage(op, rest[:i], holes); err != nil {
				return Statement{}, false, err
			}
		}
		if i == len(rest) {
			if op.MinArgs > 0 {
				return Statement{}, false, &Error{Op: name, Msg: "directive " + name + " needs a payload", Usage: op.Usage}
			}
			break
		}
		if len(op.RestValue) > 0 {
			return Statement{}, false, &Error{
				Op:    name,
				Msg:   name + " takes its program via --" + op.RestValue[0] + ", not a bare payload: " + name + " <target> --" + op.RestValue[0] + " <command> [args…]",
				Usage: op.Usage,
			}
		}
		start, end := rest[i].start, bodyEnd
		if op.payloadStopsAtWord() {
			// An interpreted payload is a NAME, not a command line: it stops at
			// that word, because more words follow it (`alias jira mcp open …`).
			end = int(rest[i].end)
			out.Args = append(out.Args, texts(rest[i+1:])...)
		}
		out.Payload = src[start:end]

	case KindFlags:
		for i := 0; i < len(rest); i++ {
			w := rest[i]
			if !strings.HasPrefix(w.text, "--") {
				out.Args = append(out.Args, w.text)
				continue
			}
			key, val := splitFlag(w.text)
			if !allowedFlag(op, key) {
				msg := "unknown flag for " + name + ": --" + key
				if hint, ok := op.RetiredFlags[key]; ok {
					msg += "\n" + hint
				}
				return Statement{}, false, &Error{
					Op:    name,
					Msg:   msg,
					Usage: op.Usage,
				}
			}
			/* A RestValue flag takes the REST of the line, exactly as it does for
			   the payload kinds: its value is a word list (`--args sh -c 'a,b'`),
			   and stopping at the first word would drop the program's arguments
			   without an error — the silent drop this flag list exists to avoid.
			   It must come last, and a known flag after it is refused rather than
			   handed to the program. */
			if restValueFlag(op, key) {
				if i+1 >= len(rest) || strings.HasPrefix(rest[i+1].text, "--") {
					return Statement{}, false, &Error{
						Op:    name,
						Msg:   "--" + key + " needs arguments, e.g. " + name + " … --" + key + " mcp",
						Usage: op.Usage,
					}
				}
				for _, later := range rest[i+1:] {
					if k, _ := splitFlag(later.text); strings.HasPrefix(later.text, "--") && allowedFlag(op, k) {
						return Statement{}, false, &Error{
							Op:    name,
							Msg:   "flags come before --" + key + ": --" + k + " would otherwise be passed as an argument",
							Usage: op.Usage,
						}
					}
				}
				tail := src[rest[i+1].start:bodyEnd]
				if val != "" {
					val += " " + tail
				} else {
					val = tail
				}
				storeFlag(out.Flags, key, strings.TrimSpace(val))
				break
			}
			hadEquals := strings.Contains(w.text, "=")
			if val == "" && !hadEquals && !valuelessFlag(op, key) && i+1 < len(rest) && !strings.HasPrefix(rest[i+1].text, "--") {
				val = rest[i+1].lit
				hadEquals = true
				i++
			}
			if !hadEquals && !valuelessFlag(op, key) && val == "" {
				out.Flags[key] = nil
			} else {
				storeFlag(out.Flags, key, val)
			}
			if err := checkValueWasNotCutOff(op, key, val, rest, i, holes); err != nil {
				return Statement{}, false, err
			}
		}
	}

	// A REFERENCE THAT LANDED IN A POSITIONAL WORD WAS REFUSED A ROLE (issue #198).
	//
	// Reaching here with a reference in Args means the word it was meant to be could
	// not become it: a kind, a flag name and a key name are matched against a table
	// BEFORE the value exists, and a channel index is folded before it too. What is
	// left is an argument the verb may never read — silently, which is how a command
	// changes its subject.
	//
	// DataArgs is the per-directive whitelist, counted from the end. Everything in
	// front of it is grammar.
	if first := len(out.Args) - op.DataArgs; first > 0 {
		for i := 0; i < first; i++ {
			if !isWholeReference(out.Args[i]) {
				continue
			}
			// NAME THE FLAGS, because "this word is not data" is only half the
			// answer: a caller who put a value here was trying to reach one of them.
			hint := []string{"this word is read by nobody — " + name + " takes its object from the target"}
			if len(op.Flags) > 0 {
				// The declaration carries the value's domain (`cursor:number`); the
				// spelling the caller types is the name alone.
				flag := strings.SplitN(op.Flags[0], ":", 2)[0]
				hint = append(hint, "a value goes after its flag:  "+name+" <target> --"+flag+" $1")
			}
			if op.NoPositional {
				hint = append(hint, "there is no positional word here at all")
			}
			return Statement{}, false, referenceIsGrammar(op, "in a positional word this verb does not read", hint...)
		}
	}

	if op.NoPositional && len(out.Args) > 0 {
		return Statement{}, false, &Error{
			Op: name,
			Msg: name + " takes no positional words — its object is the TARGET and its detail " +
				"is a KIND and that kind's key, so the rest of the line (" + strings.Join(out.Args, " ") + ") " +
				"would be read by nobody.\n" +
				"It is refused rather than ignored because a word that goes unread can change what the " +
				"command acts on: `close <session> 99` closed the whole session that way, not channel 99.\n" +
				"If the word was meant to be an index, it belongs in the address: `" + name + " <session>:99`.",
			Usage: op.Usage,
		}
	}

	if op.Kind == KindFlags && op.MaxArgs > 0 && len(out.Args) > op.MaxArgs {
		return Statement{}, false, &Error{
			Op:    name,
			Msg:   name + " takes its words as a flag, not as bare arguments — the rest of the line was \"" + strings.Join(out.Args, " ") + "\". Use --argv.",
			Usage: op.Usage,
		}
	}

	if op.Kind == KindFlags && len(out.Args) < op.MinArgs {
		return Statement{}, false, &Error{
			Op:    name,
			Msg:   "directive " + name + " needs " + fmt.Sprint(op.MinArgs) + " argument(s), got " + fmt.Sprint(len(out.Args)),
			Usage: op.Usage,
		}
	}
	return out, true, nil
}
