package directive

import (
	"fmt"
	"strconv"
	"strings"
)

// Subst is the caller's substitution table: named values from `vars`, positional
// values from `args`.
//
// THE TWO ARE ONE ENGINE WITH TWO ROLES, and the role is what decides how a value
// is inserted:
//
//	vars  ($NAME, ${NAME})  the DATA channel — the value becomes one shell word
//	args  ($1,   ${1})      the SYNTAX channel — inside a payload the value is
//	                        spliced in verbatim, because it IS a command line
//
// That asymmetry is the whole point. A caller wanting to send `pwd; whoami` to a
// terminal is not sending data with a semicolon in it, it is sending two commands;
// forcing that through the data channel quoted the value into one word and the
// shell answered "command not found: pwd; whoami". A caller filling `--data` is the
// opposite case, and quoting is exactly right there.
//
// Positional references OUTSIDE a payload (a flag value, a target) are the data
// channel too: only the payload region is shell syntax.
type Subst struct {
	Named map[string]string
	Pos   []string
}

func (s Subst) empty() bool { return len(s.Named) == 0 && len(s.Pos) == 0 }

// spanMode is how a value is inserted into one region of cmd.
//
// THE THREE MODES ARE THE WHOLE MODEL, and collapsing them into one enum is what
// keeps the rule in one place. They were three boolean fields (perWord/target/frame)
// and two independent traversal functions, which is how a "does this reference count
// as syntax" question ended up answered twice — once for the value's quoting and once
// for whether a `$` inside single quotes is even looked at.
//
// The modes differ because the DESTINATIONS differ, not because the code paths do:
//
//	a shell command line (run/type)  quoting is the shell's, so a value can be data
//	a word list (--argv, alias)      each word is syntax, the rest is boundaries
//	an identifier (target)           never a shell word, so never quoted
//	a protocol frame (cdp/jrpc/mcp)  quotes are JSON syntax, so never shell's
type spanMode int

const (
	// spanCommandLine is a payload that becomes a command line. A reference counts
	// as syntax only when it IS the span: `run h:1 $1` hands the terminal what the
	// caller passed, while `run h:1 $1 && echo done` names a WORD inside a line the
	// caller wrote, and splicing there would turn one command into two.
	spanCommandLine spanMode = iota

	// spanWords is a payload of several words where each whole word may be a
	// reference: an argv list (`--argv $1 $2`), and an alias's expansion, whose words
	// are the next directive line.
	spanWords

	// spanVerbatim is everything a shell never reads: a target (an identifier such as
	// `<session>:<n>`) and a protocol frame (JSON-RPC). Every reference inside is
	// inserted as the caller's own bytes.
	//
	// Quoting either of these was a real defect rather than a missed nicety: quoting a
	// target produced `info 's:1'`, a Path echo nobody wrote; quoting a value inside a
	// frame produced `{"e":"'2+3'"}`, a JSON string carrying shell quotes, and under
	// shell rules a reference inside the frame's own single quotes was not expanded at
	// all — while the frame directives' help says to quote the frame.
	spanVerbatim
)

// span is a half-open byte range of cmd with the insertion mode that governs it.
type span struct {
	start, end int
	mode       spanMode
}

// admits reports how a reference at [start,end) inside this span is inserted.
//
// The second return value is whether this span has an opinion at all: a reference
// outside every span falls back to the data rule (one quoted word), which is what a
// flag value is.
func (s span) admits(src string, start, end int) (mode spanMode, inside bool, raw bool) {
	switch s.mode {
	case spanVerbatim:
		// Covers, not equals: a frame is one region holding many references, and a
		// target's own bounds are exactly the reference that filled it.
		if start >= s.start && end <= s.end {
			return s.mode, true, true
		}
		return 0, false, false

	case spanWords:
		if start < s.start || end > s.end {
			return 0, false, false
		}
		// A whole WORD: the reference stands alone, so the value replaces that word
		// and nothing else.
		if (start == s.start || isSpaceByte(src[start-1])) &&
			(end == s.end || isSpaceByte(src[end])) {
			return s.mode, true, true
		}
		return s.mode, true, false

	default: // spanCommandLine
		if start == s.start && end == s.end {
			return s.mode, true, true
		}
		return s.mode, false, false
	}
}

// spanAt resolves one reference against the statement's spans, in source order.
//
// The first span that CONTAINS the reference wins, which matters where two overlap:
// a target inside a payload region is the target, not part of the command line.
func spanAt(spans []span, src string, start, end int) (mode spanMode, raw bool) {
	for _, s := range spans {
		if s.start > end {
			break
		}
		if _, inside, r := s.admits(src, start, end); inside {
			return s.mode, r
		}
	}
	// NO SPAN OWNS IT, so the data rule applies: a reference outside every payload,
	// argv or target is a flag value or a positional argument, and one quoted word is
	// what it must become.
	return spanMode(-1), false
}

// occ is one parameter expansion found by scanSubstitutions.
type occ struct {
	start, end int
	value      string
	// raw reports that the value is spliced in verbatim instead of quoted.
	raw bool
	// dq reports that the expansion sat inside double quotes, so a value that
	// must stay inside them is escaped for that context rather than re-quoted.
	dq bool
}

// substitute replaces every parameter expansion naming a value the caller supplied
// and returns the rewritten source plus the table needed to expand raw holes after
// parsing.
//
// WHY RAW HOLES SURVIVE AS SENTINELS. A raw value is arbitrary text — `pwd;
// whoami` is the example that started this — and putting it into src before parsing
// would let it end the statement, open a quote, or start a subshell: the parser
// would then read the caller's DATA as termcp GRAMMAR, which is exactly the
// confusion this channel exists to remove. So a raw hole is first written as one
// ordinary shell word (`__termcp_0__`), the parse happens against a line whose
// shape the caller's data cannot change, and only then is the hole filled.
//
// The data channel needs no sentinel: a value quoted as a single shell word is
// already a word the parser can read, and quoting it early is what keeps a value
// with a quote in it from escaping into the surrounding syntax.
func substitute(src string, sub Subst, spans []span) (string, *sentinelTable, error) {
	if sub.empty() {
		return src, nil, nil
	}
	occs, err := scanSubstitutions(src, sub, spans)
	if err != nil {
		return "", nil, err
	}
	if len(occs) == 0 {
		return src, nil, nil
	}
	table := &sentinelTable{prefix: sentinelPrefix(src)}
	var b strings.Builder
	last := 0
	for _, o := range occs {
		b.WriteString(src[last:o.start])
		switch {
		case o.raw:
			b.WriteString(table.prefix + strconv.Itoa(table.add(o.value, o.dq)) + "__")
		case o.dq:
			b.WriteString(shellDoubleQuoteBody(o.value))
		default:
			b.WriteString(shellSingleQuote(o.value))
		}
		last = o.end
	}
	b.WriteString(src[last:])
	return b.String(), table, nil
}

// scanSubstitutions walks src once, tracking the shell's own quoting, and reports
// every expansion that names a value the caller supplied.
//
// IT IS A SCAN, NOT AN AST WALK, and that is the fix for the coverage bug this
// replaces. The walker it replaced knew three node types (a call, a binary command,
// a statement), so a `$OUT` in a redirect target was never visited: defined in vars
// it was silently NOT substituted, undefined in vars it was silently left for the
// shell. Neither report was true. A linear scan has no such blind spot — a redirect
// target, a heredoc body, a loop word and a command substitution are all just bytes
// in a line, and quoting is the only thing that changes what a `$` means.
//
// Quoting follows the shell, because that is the only rule a caller can predict:
// inside single quotes nothing is expanded, inside double quotes a value is escaped
// for that context, and outside quotes it is quoted into one word.
//
// THE ONE EXCEPTION IS A VERBATIM SPAN, and it is asked as one question rather than
// special-cased per caller: a span says whether the region it covers is read by a
// shell at all (see spanMode). A protocol frame is not, so the shell's reading of its
// quotes would be a rule applied to the wrong language.
//
// The span table is passed in rather than computed here because the payload region of
// a statement is a fact the PARSER establishes (it already decided where the target
// and the flags end). Computing it twice is how the same boundary question ended up
// with two answers.
func scanSubstitutions(src string, sub Subst, spans []span) ([]occ, error) {
	var occs []occ
	var missing []string
	seen := map[string]bool{}
	inSingle, inDouble, escaped := false, false, false

	for i := 0; i < len(src); {
		b := src[i]
		if escaped {
			escaped = false
			i++
			continue
		}
		if b == '\\' && !inSingle {
			escaped = true
			i++
			continue
		}
		if b == '\'' && !inDouble {
			inSingle = !inSingle
			i++
			continue
		}
		if b == '"' && !inSingle {
			inDouble = !inDouble
			i++
			continue
		}
		name, refEnd, positional, ok := paramAt(src, i)
		if !ok {
			i++
			continue
		}
		// ONE QUESTION, ASKED ONCE: how does this reference reach the destination?
		// The span answers it, and the answer is also what says whether a `$` inside
		// single quotes is worth looking at — a verbatim region (a frame, a target)
		// has no shell to honour, so the quote state does not apply to it.
		mode, raw := spanAt(spans, src, i, refEnd)
		if b != '$' || (inSingle && mode != spanVerbatim) {
			i = refEnd
			continue
		}

		if positional {
			// Positional substitution is opt-in with the args channel: with no
			// args the caller is writing a plain shell line, and `$1` there is
			// the shell's own positional parameter (an awk script, a shell
			// function). Guessing which one was meant is the class of quiet
			// substitution this protocol keeps refusing, so the presence of args
			// is what decides which namespace `$N` lives in.
			if len(sub.Pos) == 0 {
				i = refEnd
				continue
			}
			// One-based, like the shell. `$0` is the shell's own name and has no
			// counterpart in an argument list, so it is refused rather than
			// quietly bound to the first element.
			if name == "0" {
				return nil, fmt.Errorf("args is 1-based: $0 has no argument to name, the first is $1" +
					"; to send a literal $0 to the shell, single-quote it: '$0'")
			}
			n, convErr := strconv.Atoi(name)
			if convErr != nil {
				i = refEnd
				continue
			}
			if n > len(sub.Pos) {
				return nil, fmt.Errorf("cmd references $%s but args has %d element(s); "+
					"args is 1-based and has no trailing sentinel slot", name, len(sub.Pos))
			}
			occs = append(occs, occ{
				start: i, end: refEnd,
				value: sub.Pos[n-1],
				raw:   raw,
				dq:    inDouble,
			})
			i = refEnd
			continue
		}

		value, found := sub.Named[name]
		if !found {
			if len(sub.Named) == 0 {
				// No vars: the shell owns every $NAME, as it always has.
				i = refEnd
				continue
			}
			// vars IS in use, so an undefined name is a mistake in one of the two
			// inputs rather than a request to expand something remotely. Naming it
			// before anything runs is the only report the caller can act on.
			if !seen[name] {
				seen[name] = true
				missing = append(missing, name)
			}
			i = refEnd
			continue
		}
		occs = append(occs, occ{
			start: i, end: refEnd,
			value: value,
			// A NAMED VALUE IS DATA BY DEFAULT, and only a VERBATIM region changes
			// that. Quoting is what makes a value one word for a shell; a frame is not
			// read by a shell, so the same quoting corrupts it (`{"e":"'2+3'"}`).
			//
			// A command-line payload does NOT relax it, even when the reference is the
			// whole payload: `run h:1 $CMD` is a whole payload, but the caller chose the
			// data channel and splicing it unquoted would turn its semicolons into
			// syntax.
			raw: raw && mode == spanVerbatim,
			dq:  inDouble,
		})
		i = refEnd
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("vars does not define %s; pass it in the `vars` argument, or drop the $ so the shell expands it",
			"$"+strings.Join(missing, ", $"))
	}
	return occs, nil
}

// paramAt reads one parameter expansion starting at src[i] (which is a `$`).
//
// It reports only the forms this protocol owns: a name, or a positional index.
// Everything else — `$$`, `$?`, `$@`, `$(…)`, `${x:-y}`, `${#x}` — is the shell's
// and is left untouched, because rewriting a construct whose meaning depends on a
// variable this server never sees would be a guess.
func paramAt(src string, i int) (name string, end int, positional bool, ok bool) {
	if i+1 >= len(src) {
		return "", 0, false, false
	}
	c := src[i+1]
	if c == '{' {
		rel := strings.IndexByte(src[i+2:], '}')
		if rel < 0 {
			return "", 0, false, false
		}
		inner := src[i+2 : i+2+rel]
		end = i + 3 + rel
		switch {
		case inner != "" && allDigits(inner):
			return inner, end, true, true
		case isIdent(inner):
			return inner, end, false, true
		}
		return "", 0, false, false
	}
	// ONE digit, deliberately. The shell reads `$10` as `$1` followed by `0`, and
	// borrowing that quirk would make an eleven-argument call depend on a spelling
	// nobody remembers. `${10}` is the unambiguous form, so `$N` stops at one digit.
	if c >= '0' && c <= '9' {
		return string(c), i + 2, true, true
	}
	if isIdentStart(c) {
		j := i + 1
		for j < len(src) && isIdentPart(src[j]) {
			j++
		}
		return src[i+1 : j], j, false, true
	}
	return "", 0, false, false
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// sentinelPrefix picks a word that appears nowhere in src, so a raw hole can never
// be confused with bytes the caller wrote.
func sentinelPrefix(src string) string {
	p := "__termcp_"
	for strings.Contains(src, p) {
		p = "_" + p
	}
	return p
}

// sentinelTable is the raw holes of ONE call, by index.
//
// It is per call and not global: ids mean nothing outside the source they were
// minted for, and a shared table would let a sentinel from one caller's cmd name
// another caller's value.
type sentinelTable struct {
	prefix string
	vals   []sentinelValue
}

type sentinelValue struct {
	value string
	dq    bool
}

func (t *sentinelTable) add(value string, dq bool) int {
	t.vals = append(t.vals, sentinelValue{value: value, dq: dq})
	return len(t.vals) - 1
}

// expand fills every raw hole in s, leaving the rest of the bytes alone.
//
// A value inserted here is NEVER rescanned: the walk is single-pass and a
// substituted value is written straight to the output. A caller whose argument
// happens to contain `__termcp_0__` therefore gets its own bytes back rather than
// having them read as another hole.
func (t *sentinelTable) expand(s string) string {
	if t == nil || !strings.Contains(s, t.prefix) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, t.prefix)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		rest := s[i+len(t.prefix):]
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		id, convErr := -1, error(nil)
		if j > 0 && strings.HasPrefix(rest[j:], "__") {
			id, convErr = strconv.Atoi(rest[:j])
		}
		if convErr != nil || id < 0 || id >= len(t.vals) {
			// Not one of ours: it is the caller's own bytes, written back.
			b.WriteString(t.prefix)
			s = rest
			continue
		}
		v := t.vals[id].value
		if t.vals[id].dq {
			v = shellDoubleQuoteBody(v)
		}
		b.WriteString(v)
		s = rest[j+2:]
	}
}

// expandStatement fills the raw holes of one parsed statement.
//
// It runs AFTER parsing, so the Statement's fields already hold the sentinels the
// parser was allowed to see: the payload, the Path echo, the target, the positional
// args and every flag value all come through here.
func (t *sentinelTable) expandStatement(st Statement) Statement {
	if t == nil {
		return st
	}
	st.Payload = t.expand(st.Payload)
	st.Path = t.expand(st.Path)
	st.Target = t.expand(st.Target)
	for i, a := range st.Args {
		st.Args[i] = t.expand(a)
	}
	for k, v := range st.Flags {
		if v == nil {
			continue
		}
		filled := t.expand(*v)
		st.Flags[k] = &filled
	}
	return st
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func isIdent(s string) bool {
	if s == "" || !isIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isIdentPart(s[i]) {
			return false
		}
	}
	return true
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}
