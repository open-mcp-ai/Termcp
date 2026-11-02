package directive

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Subst is the caller's substitution table: named values from `vars`, positional
// values from `args`.
//
// THE TWO ARE ONE MECHANISM WITH TWO SPELLINGS — a named variable and an indexed
// variable — and they are inserted by the SAME rule. What decides how a value is
// inserted is WHO READS IT, never which spelling named it:
//
//	a whole payload          the value IS the line, spliced verbatim (semicolons are
//	                         syntax, because a command line is what the caller sent)
//	anywhere inside a payload one quoted shell word (the value is data)
//	a destination no shell   the caller's own bytes, unquoted (a flag value, a frame:
//	reads                  nothing would take quotes off, so quotes would be bytes
//	                         the caller never sent)
//
// So `run h:1 $1` and `run h:1 $CMD` are the same request, and a caller wanting one
// word asks for one word the way it would in a shell: `run h:1 "$CMD"`.
//
// They used to differ in exactly the whole-payload case — `$1` spliced, `$CMD` quoted
// — which made identical text behave differently depending on which table it came
// from, with nothing to say so. The asymmetry was justified as a data channel versus a
// syntax channel; it is really one channel with two names for the same slot, and the
// position already says which of the three rules applies.
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
	// all — while the frame directives' help used to tell callers to quote the frame.
	spanVerbatim

	// THERE WAS A FOURTH MODE HERE — `spanArithmetic`, the inside of `$(( ))`, where a
	// quoted word is not an operand. It is gone with the region that produced it: the
	// whole-word rule refuses a reference glued to the operators around it, so no
	// accepted statement ever consulted it. See the note in analyzeSource.
)

// span is a half-open byte range of cmd with the insertion mode that governs it.
type span struct {
	start, end int
	// payloadEnd is where the PAYLOAD ends, which is not always where the REGION ends.
	//
	// A command line runs to the end of the statement — a redirect target is part of it, and
	// the shell reads it — while the payload the caller named is the words up to the first
	// redirect or pipe. The two questions are different and were sharing one number:
	//
	//	run h:1 echo hi > $1   the region holds `> $1` (so `$1` is shell, and quoted)
	//	run h:1 $1             the payload IS the reference (so the value is the line)
	//
	// With one number, `> $1` fell outside the region and took the flag-value rule: bare.
	payloadEnd int
	mode       spanMode
}

// admits reports how a reference this span CONTAINS is inserted.
//
// CONTAINMENT AND INSERTION ARE TWO QUESTIONS, and answering them in one function is what
// made "the reference is not the whole payload" look like "no region owns this reference".
// The first means "quote it"; the second means "it is a flag value, and a flag value is not
// shell". Both were reported as a false `inside`, so the two collapsed into one and a
// reference sitting beside a word in a payload took the flag-value rule.
//
// spanAt has already established containment; this is only the insertion rule for the
// region the reference is in.
func (s span) admits(src string, start, end int) (mode spanMode, raw bool) {
	switch s.mode {
	case spanVerbatim:
		// A shell never reads these, so the value is the caller's own bytes.
		return s.mode, true

	case spanWords:
		// A whole WORD: the reference stands alone, so the value replaces that word and
		// nothing else.
		whole := (start == s.start || isSpaceByte(src[start-1])) &&
			(end == s.end || isSpaceByte(src[end]))
		return s.mode, whole

	default: // spanCommandLine
		// THE SHELL'S OWN RULE: a reference is syntax only when it IS the payload.
		return s.mode, start == s.start && end == s.payloadEnd
	}
}

// spanAt reports which region owns a reference, innermost first.
//
// THE INNERMOST CONTAINING SPAN WINS. Regions nest — an arithmetic expansion sits inside
// the payload that carries it, a target sits inside the words of the statement naming it —
// and in every nesting it is the smaller, more specific region that says how the value is
// read.
//
// A range that no span contains returns spanMode(-1), and THAT IS A REGION TOO: a flag value
// (`--data`, `--name`) is outside every payload, argv list and target, and it is not shell
// syntax. Callers use this answer to tell shell from not-shell rather than re-deriving it.
func spanAt(spans []span, src string, start, end int) (mode spanMode, raw bool) {
	best := -1
	for i, s := range spans {
		if start < s.start || end > s.end {
			continue
		}
		if best < 0 || s.start > spans[best].start {
			best = i
		}
	}
	if best < 0 {
		return spanMode(-1), false
	}
	return spans[best].admits(src, start, end)
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
// ordinary shell word (a random one, see sentinelPrefix), the parse happens against
// a line whose
// shape the caller's data cannot change, and only then is the hole filled.
//
// The data channel needs no sentinel: a value quoted as a single shell word is
// already a word the parser can read, and quoting it early is what keeps a value
// with a quote in it from escaping into the surrounding syntax.
func substitute(src string, sub Subst, spans []span, refs []ref) (string, *sentinelTable, usage, error) {
	if sub.empty() {
		return src, nil, usage{}, nil
	}
	occs, used, err := resolve(src, sub, spans, refs)
	if err != nil {
		return "", nil, usage{}, err
	}
	if len(occs) == 0 {
		return src, nil, used, nil
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
	return b.String(), table, used, nil
}

// ref is one parameter expansion the PARSER reported, with the quoting context read off
// the node it sits inside.
//
// THE GRAMMAR IS THE ONLY THING THAT DECIDES WHAT A REFERENCE IS, and this is what it
// says. The pass that used to answer the same question byte by byte — toggling a flag on
// every quote it met — was a second opinion about a fact the parser had already
// established, and every disagreement between the two was a defect: a `&` trimmed off a
// command line, a `$(( ))` operand wrapped in quotes, a heredoc body quoted, a
// `--data '$1'` silently not substituted.
type ref struct {
	start, end int
	name       string
	positional bool
	// dq reports that the expansion sits inside double quotes, so a value that must stay
	// inside them is escaped for that context rather than re-quoted.
	dq bool
}

// resolve reports every reference that names a value the caller supplied.
//
// TWO SOURCES, ONE PER KIND OF REGION, and that is the whole model:
//
//   - A SHELL REGION — a payload command line, an argv list — is read by a shell, so the
//     grammar owns it: the references are the parser's nodes, and a `$` the parser did not
//     report is not one. `echo '$1'` is the shell's literal, because the parser says so.
//   - EVERY OTHER REGION — a protocol frame, a target, a flag value, a heredoc body — is
//     not read by a shell at all, so the BYTES own it. `call h '{"e":"$1"}'` and
//     `--data '$1'` are references even though they are inside the caller's single quotes,
//     because in those destinations a quote is a byte rather than syntax.
//
// The second half needs a scan because the grammar cannot see into a single-quoted string
// (its text is one field of the node), and that blindness is correct — for shell. The scan
// is confined to the regions where quoting means nothing, so it has no quote state to get
// wrong.
func resolve(src string, sub Subst, spans []span, refs []ref) ([]occ, usage, error) {
	r := resolver{
		src: src, sub: sub, spans: spans,
		seen: map[string]bool{}, reported: map[int]bool{},
		usedPos: map[int]bool{}, usedNamed: map[string]bool{},
	}
	for _, ref := range refs {
		r.reported[ref.start] = true
		if err := r.one(ref.start, ref.end, ref.name, ref.positional, ref.dq); err != nil {
			return nil, usage{}, err
		}
	}
	// The references the grammar cannot see, everywhere a shell will not read.
	r.blind()
	if r.failed != nil {
		return nil, usage{}, r.failed
	}
	if len(r.missing) > 0 {
		return nil, usage{}, fmt.Errorf("vars does not define %s; pass it in the `vars` argument, or drop the $ so the shell expands it",
			"$"+strings.Join(r.missing, ", $"))
	}
	sort.Slice(r.occs, func(i, j int) bool { return r.occs[i].start < r.occs[j].start })
	return r.occs, usage{Pos: r.usedPos, Named: r.usedNamed}, nil
}

// usage records WHICH of the caller's values the payload actually referenced.
//
// It exists because the other direction was already an error and this one was silent: a
// `$NAME` with no value in vars is refused ("vars does not define $X"), while a value with
// no `$` naming it was accepted and dropped. A caller — or a model — that passes three
// values and references one believes all three arrived, and the reply says nothing.
type usage struct {
	Pos   map[int]bool
	Named map[string]bool
}

// resolver is the state one pass needs: the inputs, and what has been reported.
type resolver struct {
	src      string
	sub      Subst
	spans    []span
	occs     []occ
	missing  []string
	seen     map[string]bool
	reported map[int]bool
	failed   error
	// What the payload referenced, so the caller can be told what it did not.
	usedPos   map[int]bool
	usedNamed map[string]bool
}

// blind reads the one kind of region the grammar does not read at all: an OPAQUE PAYLOAD.
//
// THE RULE THE WHOLE PASS FOLLOWS: a reference is a `$` the grammar reported. What the
// grammar did not report, the caller deliberately kept from being parsed — `'$1'` is the
// shell's literal, and an AI writing a shell-flavoured line knows exactly why. Substituting
// it would be this daemon overruling the quoting the caller wrote.
//
// An opaque payload is the one exception, and it is not a rule of its own: `call` takes its
// frame as KindPayload, which the grammar never interprets (a JSON frame is not a shell
// word — its braces are not even legal syntax). So "the grammar did not report it" carries
// no meaning there, and the bytes are the only truth. That is why this scans a span's bytes
// and no other: a span whose mode is spanVerbatim is a declaration that nobody parses it.
func (r *resolver) blind() {
	for _, sp := range r.spans {
		if sp.mode != spanVerbatim {
			continue
		}
		for i := sp.start; i < sp.end && i < len(r.src); {
			switch r.src[i] {
			case '\\':
				i += 2
				continue
			case '$':
			default:
				i++
				continue
			}
			name, end, positional, ok := paramAt(r.src, i)
			if !ok {
				i++
				continue
			}
			if !r.reported[i] {
				if err := r.one(i, end, name, positional, false); err != nil {
					r.failed = err
					return
				}
			}
			i = end
		}
	}
}

// one resolves a single reference, or explains why it cannot.
func (r *resolver) one(start, end int, name string, positional, dq bool) error {
	mode, raw := spanAt(r.spans, r.src, start, end)
	// A shell region is one the parser read as a command line or a word list; everything
	// else — a frame, a target, a flag value, a heredoc body — is bytes.
	shell := mode == spanCommandLine || mode == spanWords

	// DSL AND SHELL ARE NEVER MIXED.
	//
	// The shell line is a VALUE that fills a hole; it is not written around the hole. So a
	// variable that sits BESIDE shell text in a payload (`run h:1 cat $1`) is refused,
	// because there is no honest way to insert it: as data it becomes one quoted word and
	// the caller's statement is quietly changed, as syntax it turns the caller's DATA into
	// commands. Both answers are wrong, and the caller is writing a shape the protocol does
	// not have.
	//
	// `--argv` is not this case. Its word list is a DSL flag's value, not a shell line: a
	// reference there is one word of an argv, which is exactly what a variable is for.
	// The distinction is the REGION, not the spelling — which is why this reads `mode`
	// rather than testing the text for a `$`.
	if mode == spanCommandLine && !raw {
		return fmt.Errorf("a variable cannot sit inside a shell line: %s is mixed with other shell text. "+
			"termcp keeps DSL and shell apart — the whole shell command is a VALUE, so put it in the variable and "+
			"leave the reference alone in cmd:\n"+
			"  cmd: \"run <channel> $1\"   args: [\"cat /etc/hosts | head -5\"]\n"+
			"If the line is literal (no values), write it directly: cmd: \"run <channel> cat /etc/hosts\"", name)
	}

	// ESCAPING IS A PROPERTY OF THE DESTINATION, NOT OF THE QUOTES.
	//
	// `dq` says the reference sat inside double quotes in cmd, which is a fact about the
	// SOURCE. It only means anything when a shell will later read those quotes and take them
	// off: a value destined for a file path, a target or a JSON frame is the caller's bytes,
	// and escaping it injects backslashes nobody asked for.
	//
	// The reported case: `write <session> file "$1"` — the ordinary instinct to quote a path
	// — answered 202 and created a file whose NAME contained the escape sequences
	// (`C:\\Users\\...\\asked.bin`), because shell escaping was applied to a destination no
	// shell reads. The unquoted `$1` was right all along, which is the shape of the bug: two
	// spellings of the same intent, one of them silently corrupt.
	//
	// `run h:1 "$1"` is the case the flag exists for and it keeps its escaping: the payload
	// IS read by a shell, the quotes reach it, and the value has to survive that unquoting.
	escapes := dq && shell

	if positional {
		// Positional substitution is opt-in with the args channel: with no args the caller
		// is writing a plain shell line, and `$1` there is the shell's own positional
		// parameter (an awk script, a shell function). Guessing which one was meant is the
		// class of quiet substitution this protocol keeps refusing, so the presence of args
		// is what decides which namespace `$N` lives in.
		if len(r.sub.Pos) == 0 {
			return nil
		}
		// One-based, like the shell. `$0` is the shell's own name and has no counterpart in
		// an argument list, so it is refused rather than quietly bound to the first element.
		if name == "0" {
			return fmt.Errorf("args is 1-based: $0 has no argument to name, the first is $1" +
				"; to send a literal $0 to the shell, put the line in the `args` argument")
		}
		n, err := strconv.Atoi(name)
		if err != nil {
			return nil
		}
		if n > len(r.sub.Pos) {
			return fmt.Errorf("cmd references $%s but args has %d element(s); "+
				"args is 1-based and has no trailing sentinel slot", name, len(r.sub.Pos))
		}
		r.usedPos[n] = true
		r.occs = append(r.occs, occ{
			start: start, end: end,
			value: r.sub.Pos[n-1],
			// OUTSIDE A SHELL REGION NOTHING QUOTES THE VALUE, because nothing will unquote
			// it: a flag value arrives at its destination as the bytes between the flags,
			// and shell quoting there would be bytes the caller never sent.
			raw: raw || !shell,
			dq:  escapes,
		})
		return nil
	}

	value, found := r.sub.Named[name]
	if !found {
		if len(r.sub.Named) == 0 {
			// No vars: the shell owns every $NAME, as it always has.
			return nil
		}
		// vars IS in use, so an undefined name is a mistake in one of the two inputs rather
		// than a request to expand something remotely. Naming it before anything runs is the
		// only report the caller can act on.
		if !r.seen[name] {
			r.seen[name] = true
			r.missing = append(r.missing, name)
		}
		return nil
	}
	r.usedNamed[name] = true
	r.occs = append(r.occs, occ{
		start: start, end: end,
		value: value,
		// A NAMED VALUE FOLLOWS THE SAME RULE AS A POSITIONAL ONE.
		//
		// `vars` and `args` are one mechanism with two spellings: a named variable and an
		// indexed variable. They used to differ in exactly one case — a reference that IS
		// the whole payload — where `$1` was spliced verbatim and `$CMD` was quoted into a
		// single word. Two spellings of the same thing behaved differently, so a caller
		// that moved a value from `args` to `vars` (or back) got a different command line
		// from identical text, and nothing said so.
		//
		// The rule is now one rule: WHO READS THE VALUE decides how it is inserted. A
		// whole payload is a command line, so the value is the line; anywhere inside a
		// payload it is one quoted word; a destination no shell reads (a flag value, a
		// frame) gets the caller's bytes. Quoting something as a single word is a thing
		// the caller asks for with quotes (`run h:1 "$CMD"`), the same way it would in a
		// shell — not something the choice of variable spelling decides.
		raw: raw || !shell,
		dq:  escapes,
	})
	return nil
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

// sentinelMarker is the one fixed part of a hole's name: enough to RECOGNIZE a hole
// that leaked (see isSentinel), not enough to forge one.
//
// The rest is random per call. A fixed prefix made the token predictable, and a
// predictable token is a value a caller can type: `write h file __hole_...__` was a
// legal path, so "this is a hole" and "this is what the caller wrote" were the same
// string. Random bytes per call make the two distinguishable by construction.
const sentinelMarker = "__hole_"

// sentinelPrefix mints the word that stands in for a raw hole in one call's cmd.
//
// IT IS RANDOM PER CALL, and it has to be a single ordinary shell word: the parse runs
// against a line the caller's data cannot change the shape of, so the hole has to look
// like a word the grammar reads and nothing else.
//
// The collision check is kept even though 72 random bits make a collision unreachable:
// `expand` finds holes by scanning for this prefix, so "the prefix does not appear in
// src" is the invariant it relies on, and an invariant worth stating is worth
// enforcing.
func sentinelPrefix(src string) string {
	var raw [9]byte
	if _, err := crand.Read(raw[:]); err == nil {
		return uniquePrefix(src, sentinelMarker+hex.EncodeToString(raw[:])+"_")
	}
	// A deterministic fallback for the case where the system entropy source fails:
	// extend the marker until it appears nowhere in the source. Worse than random, still
	// correct — a hole that cannot collide is the property that matters.
	return uniquePrefix(src, sentinelMarker)
}

// uniquePrefix extends seed until src does not contain it.
//
// SEPARATE FROM THE RANDOM PART because this is the invariant `expand` relies on — it
// finds holes by scanning for the prefix — and it is the half that can be tested
// directly: no amount of randomness makes a colliding prefix safe, and no test can force
// a random one to collide.
func uniquePrefix(src, seed string) string {
	for strings.Contains(src, seed) {
		seed = "_" + seed
	}
	return seed
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
// happens to contain a token of the same shape therefore gets its own bytes back
// rather than having them read as another hole.
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

// isOneHole reports whether s is EXACTLY one hole and nothing else.
//
// This is how a payload declared `var` is checked: the caller wrote `$1`, the source
// the parser sees holds this call's stand-in token for it, and "the payload is one
// token, alone" is the same statement as "the payload is one variable reference".
func (t *sentinelTable) isOneHole(s string) bool {
	if t == nil || !strings.HasPrefix(s, t.prefix) || !strings.HasSuffix(s, "__") {
		return false
	}
	digits := s[len(t.prefix) : len(s)-2]
	if digits == "" {
		return false
	}
	id, err := strconv.Atoi(digits)
	return err == nil && id >= 0 && id < len(t.vals)
}

// expandStatement fills the raw holes of one parsed statement.
//
// It runs AFTER parsing, so the Statement's fields already hold the sentinels the
// parser was allowed to see: the payload, the Path echo, the target, the KEY, the
// positional args and every flag value all come through here.
//
// THE KEY WAS MISSING, AND A LEAKED SENTINEL IS A WRONG ADDRESS, NOT A BAD ONE.
//
// `write <session> file $1 --local-path go.mod` wrote the file to the literal path
// of the sentinel the substitution table mints for a hole, and
// answered 202 "accepted" with that name in the receipt. Nothing refused it, because
// a path is allowed to be anything; the caller found out by looking for the file
// where they asked and not finding it. Every verb that takes a sub-resource key had
// the same hole (file, forward, host, ext, alias, task, a peer address), which is why
// this is one line here rather than a guard at each of them.
//
// Key is the last of them: Op and Kind are declared words, and the parser refuses a
// sentinel for those by design (a hole is not a name it can match).
func (t *sentinelTable) expandStatement(st Statement) Statement {
	if t == nil {
		return st
	}
	st.Payload = t.expand(st.Payload)
	st.Path = t.expand(st.Path)
	st.Target = t.expand(st.Target)
	st.Key = t.expand(st.Key)
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
	if st.Pipe != nil {
		expanded := t.expandStatement(*st.Pipe)
		st.Pipe = &expanded
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

// spell renders a substituted string back the way the caller wrote it.
//
// It exists for ERROR TEXT ONLY. A refusal that quotes `a__hole_9f3c_0__` tells the
// caller their input contains something it does not — the marker is this parser's
// bookkeeping, and leaking it turns a teaching error into a puzzle.
func (t *sentinelTable) spell(s string) string {
	if t == nil || t.prefix == "" {
		return s
	}
	for i := range t.vals {
		s = strings.ReplaceAll(s, t.prefix+strconv.Itoa(i)+"__", "$"+strconv.Itoa(i+1))
	}
	return s
}

// gluedReference reports whether s holds a hole that is not a whole word.
//
// THE WHOLE-WORD RULE, IN ONE PLACE. A reference may be the whole word it sits in, and
// that is all: `a$1`, `/tmp/$1` and `$1/b` are one word built from two languages. The
// test is the boundary around each hole, not the shape of the whole string, because the
// string may hold SEVERAL words — a RestValue flag (`--argv sh -c $1`) is a word list,
// and each word in it is judged on its own.
//
// The marker carries a random seed, so a caller whose own text looks like a hole does
// not match this prefix and is left alone.
func (t *sentinelTable) gluedReference(s string) bool {
	if t == nil || t.prefix == "" {
		return false
	}
	for at := 0; at < len(s); {
		i := strings.Index(s[at:], t.prefix)
		if i < 0 {
			return false
		}
		i += at
		j := i + len(t.prefix)
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j+2 <= len(s) && s[j:j+2] == "__" {
			end := j + 2
			if (i > 0 && !isSpaceByte(s[i-1])) || (end < len(s) && !isSpaceByte(s[end])) {
				return true
			}
			at = end
			continue
		}
		at = i + len(t.prefix)
	}
	return false
}

// isOneWordHole reports whether s is EXACTLY one reference, quotes aside.
//
// THE WHOLE-WORD RULE, ASKED AT THE ONLY LEVEL WHERE IT HAS AN ANSWER. A word is the
// unit both grammars agree on, so the question is not "does this string contain a
// reference" but "is this string, as one word, nothing but a reference". Whitespace is
// not the test — `$(( $1 + 1 ))` has spaces around the reference and is still one word
// that means two things.
//
// A quoted reference is still a whole-word reference: the quotes are how the caller
// wrote a word, not a second language.
func (t *sentinelTable) isOneWordHole(s string) bool {
	if t == nil || t.prefix == "" {
		return false
	}
	s = trimOneQuotePair(s)
	if !strings.HasPrefix(s, t.prefix) || !strings.HasSuffix(s, "__") {
		return false
	}
	return t.isOneHole(s)
}

// trimOneQuotePair removes one matching pair of surrounding quotes, if present.
func trimOneQuotePair(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
