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
	"sort"
	"strconv"
	"strings"
)

// Kind says how the words after the target are read.
type Kind int

const (
	// KindFlags takes --name[=value] options and positional arguments.
	KindFlags Kind = iota
	// KindPayload hands every byte after the target to the directive unchanged.
	// The bytes come from the source string, so quoting survives: `'a'"b"'c'`
	// reaches the shell as `'a'"b"'c'`, not as `abc`.
	KindPayload
	// KindInterpreted is a payload that is also a NAME, not a command line
	// (`alias jira mcp open internal <url>`): the payload is one shell word, so a
	// following argument is not part of it.
	KindInterpreted
)

// TargetKind says what a directive's target names.
//
// IT IS ENFORCED AT PARSE TIME. The whole reason #131 and #132 happened is that
// every reader tried to resolve the object itself and fell back when it guessed
// wrong: `close <session>` became a primary shell close, `run <session>` became a
// primary shell run, and nobody could tell what the caller meant.
//
// Declaring the kind here turns "did you give a channel or a session?" from a
// runtime guess into a syntax check: a channel-only directive (run, key, type,
// resize, reader) REFUSES a session id before any handler runs.
type TargetKind int

const (
	// TargetAny is a target whose shape is not checked by the parser: it may be a
	// session id, an entry name, a locator or a handle. The directive resolves it.
	TargetAny TargetKind = iota

	// TargetChannel requires a channel handle: the `sid:N` short handle, a locator
	// with a non-zero index, or a raw channel id. A bare session id is REFUSED with
	// a usage line pointing at `ps` — no silent fallback to primaryShell.
	TargetChannel

	// TargetSession requires a container: a session id, an entry name, or a locator
	// naming a session.
	TargetSession

	// TargetChain names a channel in EITHER of its two spellings: the short handle
	// `a1f3:1`, or a session followed by an index, `a1f3 1`.
	//
	// THE SECOND SPELLING IS THE POINT. A channel is not an object with its own
	// existence — it is the index-th thing hanging off a session, exactly like
	// `file <path>` and `forward <id>`. Writing it as a session plus a key is what
	// makes the resource tree legible: everything under a session is addressed
	// `<session> <kind> <key>`, and for a channel the kind is the empty string and
	// the key is the index.
	//
	// The parser normalises the spaced form to the handle, so every handler below
	// still sees one spelling and no reader has to handle two.
	TargetChain
)

// Op is one directive's grammar.
type Op struct {
	Name string
	// Kind selects how the words after the target are read.
	Kind Kind
	// Target says the second word names the object the directive acts on.
	Target bool
	// TargetOptional says a target may be ABSENT, in every spelling — not only the
	// flag-only ones (`watch --off <rule>`, `watch --list`) it was introduced for.
	//
	// Without it the positional rule ("word 2 is the target") rejects a leading flag,
	// so those forms could never parse; that is why the field exists. But a targeted
	// payload verb has a third spelling with no object in it: the only remaining word
	// is the payload. `deploy $1` with `deploy [<env>] $1` reported Target="make
	// release" and an empty payload — success, no error, the value in the wrong field —
	// because the rule read `TargetOptional` in the two branches that see no target and
	// not in the one that takes a word AS one. A lone whole reference now goes to the
	// payload instead (#296).
	//
	// IT IS NOT AFFECTED BY THE OTHER DIRECTION: a required target keeps its reading,
	// and a verb that declares one and receives a value is caught by TargetKind.
	TargetOptional bool
	// TargetKind says what the target NAMES, and therefore which spellings are
	// accepted. It is a declaration, not a hint: parseStatement enforces it, so a
	// directive that needs a channel refuses a session id at parse time with a usage
	// line — before any handler runs and before any reply can claim success.
	//
	// THE ZERO VALUE IS TargetAny, and that is deliberate: a directive that does not
	// say gets the permissive reading it had before, so adding this field broke
	// nothing. The directives that DO say are the ones where the two spellings mean
	// different things, which is exactly the set that used to guess.
	TargetKind TargetKind
	// Kinds lists the sub-resource names this verb accepts in the position right
	// after the target: `read a1f3 file /etc/passwd`, `open a1f3 forward …`.
	//
	// THE KIND IS PART OF THE ADDRESS, NOT A FLAG. Everything under a session is
	// named `<session> <kind> <key>` — a file by path, a forward by id, a peer by
	// handle — and a channel by index alone, which is why the channel case needs no
	// word here. Declaring the vocabulary in the table is what lets the parser refuse
	// `read a1f3 flie /x` as a misspelling instead of silently reading "flie" as a
	// path relative to nothing.
	//
	// The first entry is the DEFAULT: a verb whose kind can be inferred from
	// TargetKind's shape (channels) lists none, and a verb that needs one lists it.
	Kinds []string
	// PayloadType says what the bytes after the target ARE, and it is the declaration
	// that keeps the directive language and the shell apart.
	//
	// THE ZERO VALUE IS "the caller's text, unread", which is what a payload meant
	// before this field existed — so nothing changed for the directives that do not
	// say. The one that says is `run` (and `input`): its payload must be EXACTLY ONE
	// VARIABLE REFERENCE ($1 or $NAME), because that is the only way the shell text
	// gets into the line as a VALUE instead of being written into the directive
	// language directly.
	//
	// WHY IT MATTERS. `run h:1 ls -la` and `run h:1 $1` with args ["ls -la"] both
	// reach the shell, but only the second one has a boundary: in the first, the
	// payload is read by the shell parser at the same time as the directive is read by
	// this one, so `run h:1 ls | write h:2 file /x` is a shell pipeline into a program
	// called `write` and `run h:1 echo a; b` is two directives. Every one of those
	// readings is a guess about which language the caller was speaking, and a guess is
	// what this refuses to make.
	PayloadType string

	// Flags lists the accepted option names (without the leading --).
	//
	// AN ENTRY MAY CARRY ITS DOMAIN: "offset:number", "stream:enum=o,e,all". The
	// annotation is part of the same list rather than a second table beside it,
	// because two lists that have to stay in step is how a declared flag ends up with
	// no declared domain and nobody notices. A name with no annotation accepts any
	// value, which is what every flag did before.
	Flags []string
	// RetiredFlags documents flags that were removed, paired with their replacement.
	// An error naming a retired flag tells the caller what to use instead of leaving
	// them with a bare "unknown flag".
	RetiredFlags map[string]string
	// MaxArgs is a CEILING on positional arguments, and 0 means "not declared".
	// It exists for the verbs whose words must arrive as a flag: `connect host sleep
	// 5` looks exactly like `ssh host sleep 5`, but the words would be dropped, so
	// they are refused instead of silently ignored.
	MaxArgs int

	// DataArgs is the WHITELIST of positional words that may hold a variable,
	// counted from the END of the word list. Zero means none of them may.
	//
	// THIS IS THE DSL ESCALATION BOUNDARY, and it is default-deny on purpose. A
	// reference is filled in after the parse, so by the time its value is known the
	// statement's SHAPE is already decided: a word that was meant to be a kind, a
	// flag name or a channel index could not become one, and it fell through to a
	// positional argument instead — where nothing reads it. `close s:1 $1` then
	// closed the WHOLE SESSION instead of channel 2 and answered 204 (issue #199).
	//
	// The words that carry data are named here (`resize <ch> <rows> <cols>` takes
	// two numbers, `get <s> cdp:1 protocol <Domain>` takes a domain). Every other
	// positional word is grammar, and a reference in one is refused by name rather
	// than silently reinterpreted.
	DataArgs int

	// NoPositional makes a stray positional word an ERROR for a verb that takes none.
	//
	// MaxArgs cannot say this: it is a ceiling where 0 means "not declared", and the
	// verbs that need this are the ones with nothing to declare. `close`/`rm`/`read`/`ls`
	// read their object from the TARGET and their detail from a KIND and its key, so a
	// leftover word is always a mistake — and ignoring it is how `close <session> 99`
	// (a channel that does not exist) closed the whole session: the index fell through to
	// a positional word, which nobody read (issue #199, second half).
	//
	// A reference that fails to become its role lands here too, so this is the backstop
	// under the DataArgs whitelist: even if a position is missed there, the word is
	// refused instead of silently changing what the command acts on.
	NoPositional bool

	// MinArgs is the minimum number of POSITIONAL arguments after the target,
	// for the flag kinds (a payload kind is governed by whether a payload exists
	// at all). It is checked so a truncated command fails here, with a usage
	// line, rather than three layers down with "shell not found".
	MinArgs int
	// Valueless lists the flags that take no value, so the word after them is
	// not swallowed as one. Every other flag consumes the next word when it was
	// not written as --name=value.
	Valueless []string
	// RestValue lists the flags whose value is the REST of the statement, not
	// one word. An argv is a word list; stopping at the first word would drop
	// the program's arguments without an error, which is the worst failure
	// mode. These flags must come last: a known flag after one is refused.
	RestValue []string
	// PayloadSpan is how a reference inside this directive's payload reaches its
	// destination. The zero value (SpanCommandLine) is the ordinary case.
	//
	// ONE FIELD, NOT TWO FLAGS. It was a pair of booleans (PerWordRaw, FramePayload)
	// and every reader had to know which won when both were set — a question the
	// declarations could not answer and nothing enforced. The mode is exclusive by
	// construction: a payload is read by a shell (quoting applies), by an argv parser
	// (whole words apply), or by a protocol peer (nothing shell-side applies).
	PayloadSpan SpanMode
	// AlsoKnownAs lists extra spellings that resolve to this directive.
	//
	// They exist for the names a caller reaches for before reading `help` — the
	// tool's own name for what a directive wraps (`extensions` for `ext`) — and
	// they are built in rather than aliases because an alias is a runtime
	// registration: a caller who has not registered one gets "unknown directive"
	// for the obvious word, which is exactly the moment the spelling is being
	// guessed. `help` prints only the canonical name, so the surface stays one
	// short list; a synonym is a way in, not a second entry.
	AlsoKnownAs []string
	// Usage is the one-line template `help` and error replies print. Window
	// parameters are deliberately absent: they are on the tool schema with
	// self-explaining names, and repeating them 22 times would be noise.
	Usage string
	// Detail is the multi-line knowledge `help <directive>` adds on request: the
	// shapes and traps a caller needs to produce something the server accepts,
	// which do not fit a one-line usage template and would be noise in an error.
	Detail string
	// Core marks the directives a caller needs before it can do anything at all:
	// open a terminal, act on it, watch it, close it. They are what an empty call
	// (the "I just woke up" guide) lists, because that reply is read by someone
	// who has not yet decided what to do — 24 lines of a capability it has no use
	// for yet is the tax this flag removes.
	//
	// Everything is still ONE call away: the guide names `help` itself, and `help`
	// lists all of them. The point is not to hide a capability, it is to put the
	// six lines a caller needs in front of the twenty-four it does not.
	Core bool
}

// NodeNote returns the note for a node, when its verbs do not tell the whole story.
func NodeNote(name string) (string, bool) {
	note, ok := nodeNotes[name]
	return note, ok
}

// KindDetail returns the detail page for a kind `open` accepts (`forward`, `mcp`,
// `cdp`, `jrpc`), when one exists.
//
// `help <kind>` derives its verb list from the usage lines, which is right, but a
// page about the KIND has no line to be derived from: it is a child, not an action.
// Without this, `help forward` answered five usage lines and none of the 54 lines
// that say which flag belongs to which direction, and the page was unreachable —
// it was written and never referenced. The map is keyed by kind and read by the
// noun branch of `help`, so a page cannot be written and then lost again.
func KindDetail(name string) (string, bool) {
	page, ok := kindDetails[name]
	return page, ok
}

// Statement is one directive after parsing.
type Statement struct {
	Op     string
	Target string
	// Kind is the sub-resource named after the target, when the verb accepts one:
	// "file", "forward", "mcp". It is empty for a channel, whose key is the index
	// and which is therefore addressed by the target alone.
	Kind string
	// Key is the sub-resource's identifier: a path for file, an id for forward, a
	// url for a peer. It is empty when Kind is.
	Key string
	// Args are the positional arguments after the target.
	Args []string
	// Flags holds --name[=value] options.
	//
	// THE POINTER IS THE POINT. A flag has three states, not two, and a plain
	// `string` can only carry two:
	//
	//	key absent        the caller did not write the flag at all
	//	nil value         the caller wrote `--name` with no value
	//	*""               the caller wrote an explicitly empty value
	//	non-empty value   the value
	//
	// With a `map[string]string` the middle three collapsed: a bare `--remote-path`,
	// one followed by an explicitly empty value, and one followed by `""` all
	// arrived as (present, ""), so every reader either treated an explicit empty
	// value as "missing" (and refused a legitimate empty `--data` that way) or
	// treated a missing value as empty (and wrote an empty file for a flag the
	// caller had fumbled). Both answers are wrong, and no reader can tell which
	// case it is looking at. A nil VALUE means the flag was given without one,
	// which every reader can now refuse by name instead of guessing.
	Flags map[string]*string
	// Payload is the raw source slice handed to the directive, for the payload
	// kinds. It is EMPTY for a directive that takes none.
	Payload string
	// Path is the directive as it will be shown back in the reply's Path header:
	// the source slice for this statement, trimmed. It is the command echo, so it
	// is the caller's own bytes, not a canonical re-rendering.
	Path string
	// Cond is how this statement relates to the one before it: "" (unconditional,
	// the first statement or one separated by `;`), "&&" or "||".
	//
	// IT CONNECTS TWO DIRECTIVES, NOT TWO SHELL COMMANDS. `&&` and `||` used to
	// fall inside a payload, so `run h:1 make && echo ok` handed the whole
	// "make && echo ok" line to the shell and the shell short-circuited it. That is
	// still what happens when the caller writes a shell line — but a shell line now
	// has to arrive through `$1` or `$FOO`, because those are the channels for
	// shell text. Everything the caller writes directly in cmd is directive
	// syntax, and `&&` between two directives means "run the next one only if this
	// one answered 2xx".
	//
	// The split is not a reparse: the substitution table turns each value into one
	// opaque token before parsing, so a `&&` inside args[0] can never be mistaken
	// for an operator here.
	Cond string

	// Pipe is the statement on the RIGHT of a `|`, when this one is on the left of
	// one. It is the one pipeline this language has, and it is deliberately narrow:
	// `read … | write …` copies bytes from one node to another WITHOUT passing them
	// through the caller's context, which is the only thing a pipeline buys here.
	//
	// WHY THIS IS NOT A GENERAL OPERATOR. `|` already means something else one layer
	// down: inside a payload it belongs to the REMOTE SHELL — `run h:1 $1 | wc -l`
	// hands the terminal a command line, and the `args` help says so. A second meaning
	// that depended on which verb was on the left would be a table the caller has to
	// memorise, and the caller is a model that will guess "shell". So anything that is
	// not `read | write` is REFUSED by name rather than silently doing one of the two.
	//
	// Before this field existed the right side was parsed and thrown away:
	// `read a1f3 | write a2b4 file /x` was ONE statement, `read`, with err == nil.
	Pipe *Statement
	// Piped is true for the statement on the right of a `|`. The dispatcher runs the
	// pair as one copy; this half on its own has no address to run against.
	Piped bool

	// Background is true when this statement was terminated with `&`, which makes it
	// PARALLEL: it runs concurrently with the statements around it instead of waiting
	// for them, and the reply still lists every slot in the order the caller wrote.
	//
	// The distinction matters because the shell parser reports `a & b` as two
	// statements and this flag is the only record that the first one was meant to
	// run in the background. Without it the line executed SEQUENTIALLY while reading
	// as parallel — a silent semantic reversal, which is worse than refusing it.
	Background bool
}

// Flag returns a flag's value and whether it was given one.
//
//	value, ok := stmt.Flag("data")
//	// ok=false: absent, OR written without a value
//	// ok=true:  an explicit value, which may be empty
//
// A reader that must tell those two apart uses FlagState: "the caller forgot the
// value" and "the caller chose an empty value" are different facts, and only one of
// them is a mistake at all.
func (s Statement) Flag(name string) (string, bool) {
	v, ok := s.Flags[name]
	if !ok || v == nil {
		return "", false
	}
	return *v, true
}

// FlagState reports which of the three states a flag is in.
//
// Each caller has to say which states it accepts; that is the point. Collapsing
// them upstream is what made an explicitly empty `--data` indistinguishable from a
// forgotten value, and what made a reader unable to say WHICH mistake the caller
// had made.
func (s Statement) FlagState(name string) FlagValue {
	v, ok := s.Flags[name]
	switch {
	case !ok:
		return FlagAbsent
	case v == nil:
		return FlagNoValue
	default:
		return FlagValueSet
	}
}

// FlagValue names the three states of a flag.
type FlagValue int

const (
	// FlagAbsent is a flag the caller did not write.
	FlagAbsent FlagValue = iota
	// FlagNoValue is a flag written without a value — `--data` with nothing after
	// it. For every flag that takes a value this is a caller mistake, and it can
	// now be reported AS that instead of as "missing".
	FlagNoValue
	// FlagValueSet is a flag with an explicit value, including an empty string.
	FlagValueSet
)

// Error is a directive-level failure: unknown op, bad flag, missing argument.
// It carries the usage line so the reply can teach, which is why it is a type
// and not a wrapped fmt.Errorf.
type Error struct {
	// Op is the directive the failure is about, or "" when the first word was
	// not a directive at all.
	Op string
	// Msg is what went wrong, in the caller's terms.
	Msg string
	// Usage is the one-line template for Op, when one is known.
	Usage string
	// Rest is the statement after Op, verbatim, so a caller-defined directive can
	// be re-parsed once its meaning is known.
	Rest string
	// TrailingSep is the operator that separated the failing statement from the
	// one before it: "&", ";", "&&", or "||".
	TrailingSep string
	// AfterPayload names the payload directive the failing statement FOLLOWED, and
	// is set only when that is the plausible cause.
	//
	// `run h:1 pwd; whoami` fails with "unknown directive: whoami", which is a true
	// statement and a useless one: the caller wrote shell commands, and the `;` that
	// separates DIRECTIVES ended the run's payload. The reply cannot fix that by
	// naming the directive table — the caller needs to know which `;` was the wrong
	// one, and that is what this field lets the reply say.
	AfterPayload string
	// Parsed is how many statements parsed successfully BEFORE this failure.
	//
	// A cmd is parsed in full before any statement runs, so a failure means none
	// of them ran — including the ones that parsed. That is the right behaviour
	// (a batch that half-ran reports a partial success the caller cannot read),
	// but the caller cannot see it from the reply: it gets a 400 and no slots,
	// which looks exactly like a batch that ran and produced nothing. Counting
	// the ones that were ready to run is what lets the reply say so.
	Parsed int
}

func (e *Error) Error() string {
	if e.Usage != "" {
		return e.Msg + "\n" + e.Usage
	}
	return e.Msg
}

// File is a parsed cmd: one or more statements, in order.
type File struct {
	Statements []Statement
	// Empty reports that cmd held nothing to run, which is the "where am I"
	// call: it is legal, cheap, and returns what is available.
	Empty bool
}

// Grammar is the directive vocabulary a parse runs against.
//
// It is a VALUE, not a global, for one reason: a program that adds its own verbs
// must not have to edit this package, and two programs in one process must not
// have to agree about what a verb means. Everything the parser knows about a
// verb — how its words are cut, what its target names, which flags it takes — is
// declared here, so "the vocabulary" and "the parse" cannot disagree.
//
// The zero value is not usable; take DefaultGrammar and append, or build one from
// your own Ops. Grammar keeps its index private so a caller cannot add a verb
// through one field and forget the other — the only way in is NewGrammar.
type Grammar struct {
	ops    []Op
	byName map[string]Op
}

// NewGrammar indexes a vocabulary, under each verb's canonical name and every
// synonym. The caller's slice is copied, so later edits to it change nothing.
func NewGrammar(ops []Op) *Grammar {
	g := &Grammar{
		ops:    append([]Op(nil), ops...),
		byName: make(map[string]Op, len(ops)),
	}
	for _, op := range g.ops {
		g.byName[op.Name] = op
		for _, alt := range op.AlsoKnownAs {
			g.byName[alt] = op
		}
	}
	return g
}

// DefaultGrammar is the vocabulary this build ships, indexed once.
var DefaultGrammar = NewGrammar(ops)

// Ops returns the vocabulary, in help order. It is a copy: a caller that appends
// to the result must not be able to reach into the table every parse shares.
func (g *Grammar) Ops() []Op {
	if g == nil {
		return nil
	}
	return append([]Op(nil), g.ops...)
}

// Names lists the verbs, sorted, for a help listing that cannot drift from the
// table.
func (g *Grammar) Names() []string {
	if g == nil {
		return nil
	}
	out := make([]string, 0, len(g.ops))
	for _, op := range g.ops {
		out = append(out, op.Name)
	}
	sort.Strings(out)
	return out
}

// Lookup returns a verb's grammar, under its canonical name or any of its
// synonyms (`extensions` for `ext`).
func (g *Grammar) Lookup(name string) (Op, bool) {
	if g == nil {
		g = DefaultGrammar
	}
	op, ok := g.byName[name]
	return op, ok
}

// The package-level forms answer from DefaultGrammar, which is what a caller that
// never mentioned a Grammar means.
func Ops() []Op                  { return DefaultGrammar.Ops() }
func Names() []string            { return DefaultGrammar.Names() }
func Lookup(n string) (Op, bool) { return DefaultGrammar.Lookup(n) }

// unusedNames lists the caller's values that cmd never referenced, sorted.
func unusedNames(sub Subst, used usage) []string {
	var names []string
	for i := range sub.Pos {
		if !used.Pos[i+1] {
			names = append(names, "$"+strconv.Itoa(i+1))
		}
	}
	for name := range sub.Named {
		if !used.Named[name] {
			names = append(names, "$"+name)
		}
	}
	sort.Strings(names)
	return names
}

// unusedMessage explains a refusal whose mirror image this check has always made:
// `vars does not define $X` fires when a `$` has no value, and this fires when a value
// has no `$`. Both are reported before anything runs, because a command that has already
// executed cannot be un-run and the caller has already acted on a table it believed had
// been applied.
func unusedMessage(sub Subst, names []string, emptyCmd bool) string {
	kind := "args and vars"
	switch {
	case len(sub.Named) == 0:
		kind = "args"
	case len(sub.Pos) == 0:
		kind = "vars"
	}
	lead := "cmd never references "
	if emptyCmd {
		lead = "cmd is empty, so it cannot reference "
	}
	return lead + strings.Join(names, ", ") + " -- the " + kind +
		" you passed hold values nothing names, and a value nothing names is dropped without a trace. " +
		"args and vars are substituted only where the grammar reports a reference, so pass exactly the values cmd names " +
		"(a table shared across several calls has to be trimmed per call), or reference this one"
}

// checkTargetKind enforces an Op's TargetKind declaration at PARSE time.
//
// It is here, in the parser, and not in the dispatch layer, because that is the one
// place a bad target can be refused with a usage line BEFORE anything runs. The two
// bugs that produced this check both came from the opposite arrangement — resolve
// first, fall back when the resolution is ambiguous:
//
//	close <session> --force   resolved to the session's primary shell, closed that,
//	                          answered 204, and left the session running (#132)
//	run <session> ls          resolved to the primary shell, which is a guess when
//	                          the session has several channels
//
// A channel target must therefore say WHICH channel. The spelling that does is
// `<session>:<index>` (what `ps` prints for every channel, index 1 included) and a
// resource locator with an index. A bare session id is none of them: it names a
// container, and a container is not a channel.
func checkTargetKind(op Op, target string) *Error {
	switch op.TargetKind {
	case TargetChannel:
		if NamesAChannel(target) {
			return nil
		}
		return &Error{Op: op.Name, Msg: ChannelTargetProblem(op, target), Usage: op.Usage}

	case TargetSession:
		// BOTH HALVES OF THE DECLARATION ARE ENFORCED, and the second one was missing.
		// TargetSession was declared and never checked, so `rm <session>:1` — a spelling
		// its own usage line does not offer — resolved the locator, found the session
		// and erased it. That is the same class of silent widening the channel check
		// exists to prevent, one direction over: a caller who named a CHANNEL asked
		// about that channel, and deleting the whole container is a bigger answer than
		// the question.
		if !NamesAChannel(target) {
			return nil
		}
		return &Error{Op: op.Name, Msg: SessionTargetProblem(op, target), Usage: op.Usage}
	}
	return nil
}
