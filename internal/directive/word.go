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

	"github.com/open-mcp-ai/termcp/internal/locator"
)

// DetectRawControl scans a command line for raw control bytes that the terminal
// would act on (submit, interrupt, suspend, EOF) rather than print.
//
// IT DOES NOT EXCLUDE QUOTED SECTIONS, and that is the correction: this used to skip
// anything inside `'…'` or `"…"` on the theory that a quoted byte is the caller's
// data. It is the caller's data to the SHELL — but the byte reaches the TERMINAL
// first, and the line editor does not parse quotes. Measured: `run h:1 echo 'a␍b'`
// with a quoted carriage return left PowerShell at its continuation prompt `>>`,
// ran something the caller never asked for, and answered 200. A quoted control byte
// is exactly as dangerous as an unquoted one, so there is no quoting rule to apply
// here — only "a raw control byte in the line is refused unless the caller says
// otherwise".
//
// The bytes checked are the C0 control set (< 0x20), excluding the two that are
// whitespace to every layer: TAB (0x09) and newline (0x0a), the latter being this
// protocol's own statement separator. Everything else — CR (0x0d), Ctrl-C (0x03),
// Ctrl-D (0x04), Ctrl-Z (0x1a), ESC (0x1b), NUL (0x00) — is a terminal action.
func DetectRawControl(src string) (byte, string, bool) {
	for i := 0; i < len(src); i++ {
		b := src[i]
		if b < 0x20 && b != '\t' && b != '\n' {
			return b, controlByteName(b), true
		}
	}
	return 0, "", false
}

// controlByteName names a control byte for the rejection message.
func controlByteName(b byte) string {
	switch b {
	case '\r':
		return "carriage return (\\r, 0x0d)"
	case 0x00:
		return "NUL byte (0x00)"
	case 0x03:
		return "Ctrl-C (0x03, interrupt)"
	case 0x04:
		return "Ctrl-D (0x04, EOF)"
	case 0x1a:
		return "Ctrl-Z (0x1a, suspend)"
	case 0x1b:
		return "ESC (0x1b, terminal escape sequence)"
	default:
		return fmt.Sprintf("control byte 0x%02x", b)
	}
}

// SessionTargetProblem is the message for a target that names a channel where a
// container was required.
//
// It names the two spellings that do what the caller probably meant, because "you
// gave me a channel" alone leaves them guessing which of close/rm applies: `close`
// takes a channel and stops it, `rm` takes a session and erases the record.
func SessionTargetProblem(op Op, target string) string {
	return op.Name + " needs a SESSION, not the channel \"" + target + "\". " +
		"A channel address names one shell inside a session; this directive acts on the " +
		"container. To stop a single channel use `close " + target + "`; to erase the " +
		"whole session use its id without the `:N` suffix (`ls` prints it in the first column)."
}

// ChannelTargetProblem is the message for a target that names a container where a
// channel was required.
//
// It is exported because the same refusal happens at two layers — the parser and
// the dispatch entry — and the two must say the same thing. It names `ls` and the
// shape rather than just rejecting, because "which channel" is answerable in one
// line and an error that only refuses leaves the caller to work it out.
func ChannelTargetProblem(op Op, target string) string {
	return op.Name + " needs a CHANNEL, not \"" + target + "\". Run `ls` and use the channel it prints (`<session>:N`); a session is the container, and which of its channels to use is not something this directive will guess."
}

// NamesAChannel reports whether a target explicitly names channels, one per entry.
//
// The rule is one comparison per entry: a handle carries a non-zero channel index.
// That is what `locator.SplitHandle` already decides for every other reader, so a
// target this function accepts is exactly a target the resolvers will read as a
// channel — there is no second spelling to keep in sync.
//
// A COMMA-SEPARATED BATCH is accepted when EVERY entry names a channel: `a:1,b:2`
// is two channels, while `a:1,b` mixes a channel with a container and is refused as
// a whole. All-or-nothing is the same rule a batch already follows everywhere else
// (one bad statement refuses the batch), and it keeps the caller from getting half
// a reply without knowing which half was guessed at.
//
// A bare session id, an entry name and a `#<session>` locator all have index 0 and
// are therefore NOT channel targets. That is the breaking change: `run <session>`
// used to work by falling back to the primary channel, and now it is refused at
// parse time.
// kindWord reports whether the next word names a sub-resource this verb accepts.
//
// Only a DECLARED word counts. A path, a url or a session id may look like anything,
// and quietly promoting one of them to a kind would turn `read a1f3 /etc/passwd`
// into "the file /etc/passwd of session a1f3" when the caller meant the channel read
// whose target they mistyped. Requiring the declaration keeps the reading obvious.
func kindWord(op Op, rest []word) (string, bool) {
	kind, _, ok := kindWordWithKey(op, rest)
	return kind, ok
}

// kindWordWithKey reads a KIND, including the two compact forms that attach a key
// to it: `<peer>:<index>` names one attached session (the same colon a channel uses
// to carry its index), and `<peer>@<targetId>` names a one-shot session on a target.
//
// THEY ARE ONE WORD BECAUSE THEY ARE ONE NODE. `a1f3:1` is a channel, not a session
// plus a suffix; `a1f3 cdp:1` is an attached page, not a peer plus a suffix. Keeping
// the key attached to the kind is what lets the address be copied out of a reply and
// pasted back into the next command.
func kindWordWithKey(op Op, rest []word) (kind, key string, ok bool) {
	if len(op.Kinds) == 0 || len(rest) == 0 {
		return "", "", false
	}
	w := rest[0].text
	if isSentinel(w) {
		return "", "", false
	}
	for _, k := range op.Kinds {
		if k == w {
			return w, "", true
		}
		if strings.HasPrefix(w, k+"@") {
			target := strings.TrimPrefix(w, k+"@")
			if target == "" {
				return "", "", false
			}
			return k, "@" + target, true
		}
		if strings.HasPrefix(w, k+":") {
			// `<kind>:<index>` names a connection; `<kind>:<index>@<target>` names one
			// target inside it. Both are ONE word because they are ONE address — the
			// split happens downstream, where the peer's own state says whether the
			// target exists.
			rest2 := strings.TrimPrefix(w, k+":")
			idx := rest2
			if at := strings.IndexByte(rest2, '@'); at >= 0 {
				idx = rest2[:at]
				if rest2[at+1:] == "" {
					return "", "", false
				}
			}
			if _, isIndex := channelIndexWord(idx); !isIndex {
				return "", "", false
			}
			return k, rest2, true
		}
	}
	return "", "", false
}

// channelIndexWord reports whether word is a bare channel index and returns it
// normalised. Only digits qualify: a channel's key is its position in the session,
// and accepting anything else would let `run a1f3 file` name a channel called "file".
func ChannelIndex(word string) (string, bool) { return channelIndexWord(word) }

func channelIndexWord(word string) (string, bool) {
	w := strings.TrimSpace(word)
	if w == "" {
		return "", false
	}
	for _, r := range w {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	if w == "0" {
		// Channel indices start at 1; 0 would be a name no reply ever prints.
		return "", false
	}
	return w, true
}

func NamesAChannel(target string) bool {
	entries := strings.Split(target, ",")
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if locator.SplitHandle(entry).Index <= 0 {
			return false
		}
	}
	// An all-empty target is not a channel either.
	return len(entries) > 0 && strings.TrimSpace(target) != ""
}

// word is a parsed argument plus its byte range in the source, so a payload can
// be SLICED instead of re-joined from parts.
//
// lit is the shell's own reading of the word — unquoted, but NOT expanded (no
// `$HOME`, no `$(cmd)`): the parser is forbidden from evaluating. A flag VALUE is
// taken from lit, because `--argv go build ./...` means the program name and its
// arguments, not a string with quotes in it; a payload is taken from the slice,
// because the shell downstream must see the quotes.
type word struct {
	text  string
	lit   string
	start uint
	end   uint
}

// texts returns the words' text, for the positional arguments that follow an
// interpreted payload.
func texts(ws []word) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.lit)
	}
	return out
}

// restOfStatement is everything after the directive word, verbatim.
//
// The slice comes from the source, so it is byte-identical to what the caller
// wrote: this is the same rule the payload follows, and it exists for the one
// statement the parser could not parse — a caller-defined directive, whose meaning
// only the alias table knows.
func restOfStatement(src string, stmt *syntax.Stmt) string {
	start, end := int(stmt.Pos().Offset()), commandEnd(stmt)
	if start < 0 || end > len(src) || start >= end {
		return ""
	}
	text := src[start:end]
	if i := strings.IndexAny(text, " 	"); i >= 0 {
		return strings.TrimSpace(text[i+1:])
	}
	return ""
}

// wordLiteral is the shell's reading of one word: the caller's own bytes with the
// quoting removed, and NOTHING else touched.
//
// The rule is "read the source slice, not the node". `syntax` offers a `Value` on
// every literal, and it is not a byte-faithful copy of what the caller typed:
//
//	quoted CRLF   Value drops the CR   ("a\r\nb" parses to a\nb)
//	quoted CR     Value keeps it
//	$((1+2))      Value does not exist at all
//
// `Value` is the parser's model of what a shell WOULD expand to, which is not what
// this protocol wants: the caller is writing a value, not asking for expansion. The
// source slice at Pos()/End() is the bytes they wrote, so that is what a flag value
// and an interpreted word are read from.
//
// What this replaced lost data in three separate ways, all silent:
//
//   - A `*syntax.DblQuoted` only copied its `*syntax.Lit` children, so anything
//     else inside the quotes vanished: `--data "$HOME/x"` became `/x`, and
//     `--data "A`core`B"` became `AB`.
//   - `partText` INVENTED text for unquoted expansions: `$B` came back as `${B}`
//     and `$((1+2))` as `…` — the parser rewriting the caller's command.
//   - `SglQuoted.Value` and `Lit.Value` drop a CR, so a quoted CRLF lost its CR.
//
// The payload never came through here (it is sliced whole), which is why `run` was
// unaffected by all three while `--data` was mangled by each.
func wordLiteral(src string, w *syntax.Word) string {
	if w == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range w.Parts {
		writeWordPart(&b, src, part)
	}
	return b.String()
}

// writeWordPart appends one part's bytes to b: quotes removed, everything else
// verbatim.
func writeWordPart(b *strings.Builder, src string, part syntax.WordPart) {
	switch p := part.(type) {
	case *syntax.DblQuoted:
		// The quotes are the caller's quoting, so they are dropped; what is INSIDE
		// them is theirs and is copied byte for byte, recursively — including the
		// parts that are not literals.
		for _, inner := range p.Parts {
			writeWordPart(b, src, inner)
		}
	case *syntax.SglQuoted:
		b.WriteString(quotedBody(src, p))
	default:
		// A literal, or a construct the parser must not evaluate ($VAR, $(cmd),
		// `cmd`, $((…)), <(…)). All of them are handed over as the caller spelled
		// them: the parser's job is to hand a value through intact, never to guess
		// at it or to rewrite it into a spelling it likes better.
		b.WriteString(sourceSpan(src, part))
	}
}

// quotedBody is a single-quoted part's bytes without its quotes.
//
// It cannot come from Value: that form drops a CR from a CRLF, so a quoted
// `a\r\nb` arrived as `a\nb` and the caller's line ending was gone.
func quotedBody(src string, q *syntax.SglQuoted) string {
	body := sourceSpan(src, q)
	// $'…' has two bytes of opening delimiter, '…' has one, and both close with one.
	open := 1
	if q.Dollar {
		open = 2
	}
	if len(body) < open+1 {
		return q.Value
	}
	return body[open : len(body)-1]
}

// sourceSpan is a node's own bytes, or "" if the range is not inside src.
//
// The bounds check is not defensive noise: a caller may Parse a slice of a larger
// string, and an out-of-range position must yield no bytes rather than panic in the
// middle of a reply.
func sourceSpan(src string, node syntax.Node) string {
	start, end := int(node.Pos().Offset()), int(node.End().Offset())
	if start < 0 || end > len(src) || start >= end {
		return ""
	}
	return src[start:end]
}

// storeFlag records an explicit value.
//
// Every write goes through here or through a `nil` assignment, so the three states
// (absent / no-value / empty-value) have exactly two spellings in this file and
// neither can be produced by accident.
func storeFlag(flags map[string]*string, key, val string) {
	v := val
	flags[key] = &v
}

// splitFlag reads --name and --name=value.
func splitFlag(word string) (key, val string) {
	trimmed := strings.TrimPrefix(word, "--")
	if key, val, ok := strings.Cut(trimmed, "="); ok {
		return key, val
	}
	return trimmed, ""
}

// restValueFlag reports whether a flag's value is the rest of the statement.
func restValueFlag(op Op, key string) bool {
	for _, f := range op.RestValue {
		if f == key {
			return true
		}
	}
	return false
}

// valuelessFlag reports whether a flag takes no value.
func valuelessFlag(op Op, key string) bool {
	for _, f := range op.Valueless {
		if f == key {
			return true
		}
	}
	return false
}

// allowedFlag reports whether the directive accepts a flag.
func allowedFlag(op Op, key string) bool {
	_, _, ok := flagDomain(op, key)
	return ok
}

// PayloadVar is the one payload type this language declares: exactly one variable
// reference, so the shell line arrives as a VALUE.
const PayloadVar = "var"

// FlagNames returns the declared flags with their value-domain annotation stripped.
//
// THE ANNOTATION IS PART OF THE DECLARATION, NOT PART OF THE NAME. Writing the domain
// into the flag's own entry ("offset:number") is what keeps one list true instead of
// two, but `offset:number` is not a flag anybody can pass — and every surface that
// renders a flag name reads this table. Rendering the raw entries put `--cursor:number`
// in `help read` with an empty description beside it, because the summary table is keyed
// by the name. So the annotation is stripped at the one place names leave this package.
func (op Op) FlagNames() []string {
	out := make([]string, 0, len(op.Flags))
	for _, f := range op.Flags {
		base, _, _ := strings.Cut(f, ":")
		out = append(out, base)
	}
	return out
}

// flagDomain finds a declared flag and returns its name, its domain and whether it is
// declared at all. The domain is written into the flag's own entry ("offset:number"),
// so there is one list to keep true rather than two.
func flagDomain(op Op, key string) (name, domain string, ok bool) {
	for _, f := range op.Flags {
		base, dom, _ := strings.Cut(f, ":")
		if base == key {
			return base, dom, true
		}
	}
	return "", "", false
}

// checkParamDomains refuses a value that is outside the domain its flag declared, and
// it runs AFTER substitution so a value that arrived through $1 is checked too.
//
// IT IS NOT A REPLACEMENT FOR THE HANDLERS' OWN CHECKS, it is the layer above them:
// a handler validates what it needs to do its job, this one catches a value the
// declaration already knew was impossible — and catches it before any handler runs,
// which is the difference between "the reply explains the flag" and "the reply reports
// whatever the handler did with the wrong value".
func checkParamDomains(st Statement, op Op) error {
	for name, v := range st.Flags {
		if v == nil {
			continue
		}
		_, domain, ok := flagDomain(op, name)
		if !ok || domain == "" {
			continue
		}
		kind, values, _ := strings.Cut(domain, "=")
		switch kind {
		case "number":
			if !isNumber(*v) {
				return &Error{Op: op.Name, Msg: "--" + name + " takes a number, got " + strconv.Quote(*v), Usage: op.Usage}
			}
		case "enum":
			if !inList(*v, values) {
				return &Error{Op: op.Name, Msg: "--" + name + " takes one of " + strings.ReplaceAll(values, ",", ", ") +
					", got " + strconv.Quote(*v), Usage: op.Usage}
			}
		case "geometry":
			// --size IS `<rows>x<cols>`, and it used to be parsed by fmt.Sscanf, which
			// returns 0 for anything it does not understand — and the caller of that
			// parser then skipped the values when they were 0. So `--size nonsense`
			// started a terminal at the default size and said nothing.
			if !isGeometry(*v) {
				return &Error{Op: op.Name, Msg: "--" + name + " takes ROWSxCOLS (e.g. 40x120), got " +
					strconv.Quote(*v), Usage: op.Usage}
			}
		}
	}
	return nil
}

// isNumber accepts what the numeric flag readers accept: an integer or a decimal,
// with an optional sign. It is deliberately the same shape they parse, so this layer
// cannot pass something they will reject (or reject something they would take).
func isNumber(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return false
	}
	return true
}

// isGeometry accepts `<digits>x<digits>`, which is the one shape --size documents.
func isGeometry(s string) bool {
	rows, cols, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "x")
	if !ok {
		return false
	}
	digits := func(v string) bool {
		if v == "" {
			return false
		}
		for i := 0; i < len(v); i++ {
			if v[i] < '0' || v[i] > '9' {
				return false
			}
		}
		return true
	}
	return digits(rows) && digits(cols)
}

func inList(v, list string) bool {
	v = strings.TrimSpace(v)
	for _, allowed := range strings.Split(list, ",") {
		if v == strings.TrimSpace(allowed) {
			return true
		}
	}
	return false
}

// literalWord is a word's text as the SHELL reads it: quotes joined, nothing
// expanded. It is the same reading wordLiteral gives a flag value and an
// interpreted payload, so a word means the same thing in every position.
//
// Two things follow, and both are the point:
//
//   - Quoting a target does not change which target it names: `info 'a1f3:1'` and
//     `info a1f3:1` name the same channel. That matters because quoting is the
//     ONLY way to write a word-initial '#' — unquoted, `#` starts a comment —
//     which is how a `#<session>` locator gets typed into a shell at all.
//   - A word is never reported as the empty string. The previous reading sliced
//     the source and refused anything containing a quote, and its one caller
//     discarded the refusal, so a quoted target parsed to "" and `info '#sid'`
//     answered `Session ” not found`: the quotes silently DELETED the target.
//
// A word that cannot be read without evaluating it (`$VAR`, `$(cmd)`) comes back
// in its source spelling and then fails to resolve visibly. The payload never
// comes through here — it is sliced, because the shell downstream must see the
// quotes.
func literalWord(src string, w *syntax.Word) (string, bool) {
	if w == nil {
		return "", false
	}
	return wordLiteral(src, w), true
}

// SplitArgv turns a --argv value into the executable and its arguments.
//
// Words are read with the shell's own grammar, so quoting groups the way a
// caller expects (`sh -c "echo hi"` is three words, the third keeping its
// space), but nothing is evaluated. There is no second rule for a value that is
// one quoted word: `--argv 'go build ./...'` is ONE word, exactly as it would be
// in a terminal.
//
// A value that is a command line rather than a word list — a second statement,
// a pipeline, a redirection, an assignment — is an error, not something to feed
// to the program as text.
func SplitArgv(raw string) (string, []string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil, nil
	}
	words, err := shellWords(raw)
	if err != nil {
		return "", nil, err
	}
	if len(words) == 0 {
		return "", nil, nil
	}
	return words[0], words[1:], nil
}

// shellWords parses raw as a single simple command and returns each word's
// literal, with quotes removed and expansions left unexpanded.
func shellWords(raw string) ([]string, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(raw), "")
	if err != nil {
		return nil, fmt.Errorf("cannot read --argv: %v", err)
	}
	notArgv := fmt.Errorf("%q is a command line, not an argv: one program per --argv (use sh -c or a script for shell syntax)", raw)
	if len(file.Stmts) != 1 || file.Stmts[0].Background {
		return nil, notArgv
	}
	call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 || len(call.Assigns) > 0 || len(file.Stmts[0].Redirs) > 0 {
		return nil, notArgv
	}
	words := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		words = append(words, wordLiteral(raw, w))
	}
	return words, nil
}

// Names lists the directive names, sorted, for a help listing that cannot drift
// from the table.
func Names() []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.Name)
	}
	sort.Strings(out)
	return out
}

// hasPayload reports whether this directive takes bytes after its target at all.
//
// IT IS ASKED IN FOUR PLACES — does a statement contribute a raw span, may a payload
// be sliced here, does the error hint apply — and each of them used to spell the
// answer out as `Kind == KindPayload || Kind == KindInterpreted`. That spelling is
// what made a fourth kind a four-line change with a silent failure mode when one copy
// was missed: a directive that takes a payload but is not recognised as one simply
// gets no substitution, and nothing says so.
func (op Op) hasPayload() bool {
	return op.Kind == KindPayload || op.Kind == KindInterpreted
}

// payloadStopsAtWord reports whether a payload is one NAME rather than a command
// line, so it ends at that word instead of running to the end of the statement.
func (op Op) payloadStopsAtWord() bool { return op.Kind == KindInterpreted }

// payloadSpanMode is how a directive's payload region is substituted.
//
// A pipe or pty program takes its argv through a RestValue flag, and that value is a
// word list no matter what the directive's payload would have been — so the flag's
// own rule wins over the declaration.
func (op Op) payloadSpanMode() spanMode { return op.PayloadSpan }

// keyIsThePayload reports whether a verb's words after the kind ARE its payload.
//
// `call a1f3 cdp '{"method":"Page.enable"}'` is a peer address followed by a frame,
// and the frame is the whole rest of the statement. Reading the first word as a KEY
// (the rule for `read a1f3 file /etc/passwd`) ate the frame and then answered "needs
// a JSON-RPC frame" about the frame it had just swallowed. A payload verb therefore
// takes its key only in the attached spellings — `cdp:1`, `cdp@ABC123` — where the
// boundary is a colon or an `@`, not a space.
func (op Op) keyIsThePayload() bool {
	return op.Kind == KindPayload || op.Kind == KindInterpreted
}
