package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// The term argument table.
//
// One declaration per argument, and every place that publishes an argument reads
// it: the tool schema (term_register.go), docs/api.md's parameter table (§16, via
// cmd/termdoc), and the tests that check the two agree. Before this file they were
// three hand-kept copies, and they had already drifted: a projection switch shipped
// in the schema and was missing from the doc, so a caller reading the REST
// reference could not discover it. That argument has since been replaced (see
// structured_content below), but the drift is why this table exists — any name
// would have done, and one did.
//
// The rule this encodes is the same one the command space already follows — the
// directive table is the single source `help` renders from — extended to the
// arguments, which were the last hand-copied description on the surface.
//
// What stays hand-written here: the prose (a description is a sentence, not a
// derivation) and the parse call itself. What moved out: the defaults, the value
// ranges, and the per-argument shape the schema and the doc both need.

// argKind is the value type an argument carries.
type argKind int

const (
	argString argKind = iota
	argNumber
	argBool
	argEnum
	// argMap is a JSON object of string→string, for a parameter whose shape is a
	// dictionary rather than a scalar. It exists for `vars`: a caller that needs to
	// send data the command text cannot carry safely writes it as a named value here
	// and references the name in cmd.
	argMap
	// argStringArray is a JSON array of strings, for a parameter that is a LIST of
	// independent values rather than one blob. It exists for `args`: each element is
	// one positional value, referenced as $1, $2, … in cmd.
	argStringArray
)

// termArgDecl is one argument of the term tool.
type termArgDecl struct {
	Name string
	Kind argKind

	// Desc is the sentence a caller reads in the schema and in the docs. It is
	// written once and published verbatim in both, which is the whole point: a
	// description that lives in one place cannot disagree with itself.
	Desc string

	// DocNote is the doc's extra column, when the table has something to add that
	// the schema's one-liner cannot (a measured cost, a warning). Optional.
	DocNote string

	// Default and its rendering. Def is the value the parser uses when the
	// argument is absent; DefText is how it is spelled for a reader ("false",
	// "truncate", "8192"). They are separate because a float64 default prints as
	// 2 rather than 2.0 and an empty string default is a real, meaningful default
	// that must still be visible.
	Def     any
	DefText string

	// Enum lists the accepted values for argEnum.
	Enum []string

	// Min is the lowest value an argNumber accepts. There is no Max: every upper bound
	// this table used to carry was removed on purpose, because each one was a size or a
	// duration the CALLER pays for, and refusing it made this server overrule the only
	// party that can judge the trade. The numbers that used to be ceilings are published
	// as recommendations in Desc/DocNote instead.
	//
	// It is declared HERE, next to the default, because the three readers must not be
	// able to disagree: the schema publishes it (so a client can refuse a bad value
	// before the call), parseTermArgs enforces it (so the REST entrance, which never
	// reaches mcp-go's schema validator, refuses exactly the same values), and
	// docs/api.md renders it. Before this, the schema carried only a `default`, the
	// parser carried an if-chain of ranges, and the doc carried prose — three copies of
	// one rule.
	Min any

	// BoundNote is what a caller is told when it lands outside Min/Max, after the
	// range itself.
	//
	// A RANGE WITHOUT A REASON IS A WALL. `timeout must be <= 60, got 320` is true and
	// teaches nothing: the caller that sent 320 believed it was asking for a wait, and
	// what it needs to hear is that this window is a glance — that the reply is a slice
	// to look at and the rest is another call. That sentence lives next to the bound
	// because the bound is what provokes it, and a caller cannot be expected to read
	// the design out of a number.
	BoundNote string
}

// termArgDecls is THE table. Order is publication order: the schema and the doc
// table both read it top to bottom, so a reader scanning either one meets the
// arguments in the same sequence.
var termArgDecls = []termArgDecl{
	{
		Name:    "cmd",
		Kind:    argString,
		Desc:    "Shell-like DSL. Empty = help.",
		DocNote: "Empty or omitted returns the guide (where am I, what can I do, what is here) — never an error. A `cmd` that is present but not a string is a 400: a command that was sent and did not survive the transport is not the same request as one never sent",
		Def:     "",
		DefText: "",
	},
	{
		Name:    "timeout",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Seconds to wait for output (0 = don't wait); an upper bound, not a completion wait. Default %d; %d is the wait to stay under.", defaultTimeoutSeconds, maxWaitSeconds),
		DocNote: fmt.Sprintf("Seconds to look (default %d; a pipe returns as soon as its process exits). %d is the wait to stay under, not a wall: a longer window is allowed, and the reply says how long it waited. An upper bound on waiting, not a completion wait", defaultTimeoutSeconds, maxWaitSeconds),
		Def:     defaultTimeoutSeconds,
		DefText: strconv.Itoa(defaultTimeoutSeconds),
		Min:     0,
		// NO UPPER BOUND ON PURPOSE. It used to be refused above maxWaitSeconds, on the
		// reasoning that the transport in front of the caller gives up first, so a longer
		// window loses the reply. That is true of the transport and not of this server's
		// decision: the caller is the one who will lose the reply, it knows which client it
		// is talking through, and refusing makes the number it can say depend on a timeout
		// somewhere else. The reply already reports `Waited` and `Still-Running`, so a wait
		// that hit someone else's ceiling is visible as a fact rather than as a refusal.
		BoundNote: fmt.Sprintf("Time and space are both costs the caller pays, and neither has a hard cap here: %d seconds is the wait to stay under, not a wall.\n"+
			"So this is an upper bound on waiting, not a wait for completion: the command keeps running and `read <channel>` takes another look whenever you want one.\n"+
			"One real ceiling exists and it is not this server's: the MCP transport in front of you may give up sooner (30s on this server's SSE bridge, 15s on a peer call). Past that the reply is lost while the command runs on, and the reply's own `Waited`/`Still-Running` headers are how you tell what happened.\n"+
			"Work that outlives a call belongs on something that outlives it: `open <session> --argv <cmd>` (a pipe reports its exit), a pipeline (a copy answers with a task you poll).", maxWaitSeconds),
	},
	{
		Name:    "bytes_limit",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Bytes of the slice; 0 = channel only, else at least %d. Default %d; %d is the size to stay under.", minWindowBytes, defaultBytesLimit, windowBytesMax),
		DocNote: fmt.Sprintf("Bytes per slot (default %d; `0` = the channel only, no bytes, otherwise at least %d — a window narrower than one character cannot return one). %d is the size to stay under, not a wall: you are paying for the context, so a larger window is allowed and said so in the reply", defaultBytesLimit, minWindowBytes, windowBytesMax),
		Def:     defaultBytesLimit,
		DefText: strconv.Itoa(defaultBytesLimit),
		Min:     0,
		// NO UPPER BOUND ON PURPOSE. Time is refused above its cap because the transport
		// in front of the caller gives up first, so a longer window loses the reply; space
		// has no such wall. The caller is the one who pays for what lands in its context,
		// and it is the only party that can judge the trade — so this is a HINT with a
		// loud header when it is exceeded, not a refusal. Refusing would be termcp
		// overruling the one decision it has no basis to make.
		BoundNote: fmt.Sprintf("Time and space are different here: a long wait can lose the reply to the transport, so timeout has a hard cap — space has none. %d bytes is the size to stay under, not a wall.\n"+
			"More is usually another look rather than a bigger window: `read <channel> --cursor <n>` continues exactly where this reply stopped, and `--tail`/`--grep` pick out the part you actually want.\n"+
			"Below %d the window is refused rather than answered: a window never ends inside a character, so one too small to hold a whole one withholds those bytes and advances nothing — the same empty reply on every call.", windowBytesMax, minWindowBytes),
	},
	{
		Name:    "lines_limit",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Lines of the slice; 0 = no limit. Default %d; %d is the size to stay under.", defaultLinesLimit, windowLinesMax),
		DocNote: fmt.Sprintf("Lines per slot (default %d; `0` = no line limit). %d is the size to stay under, not a wall: a longer tail is allowed, and said so in the reply", defaultLinesLimit, windowLinesMax),
		Def:     defaultLinesLimit,
		DefText: strconv.Itoa(defaultLinesLimit),
		Min:     0,
		// Same reasoning as bytes_limit: no wall, a hint and a header.
		BoundNote: fmt.Sprintf("Space has no hard cap here, and this is a size to stay under rather than a wall: %d lines is already past what one call should carry, and a reply that exceeds it says so.\n"+
			"Select what you want with `--tail`/`--grep`, or page with `--cursor`.", windowLinesMax),
	},
	{
		Name:    "total_bytes_limit",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Cap for the whole reply. Default %d; %d is the size to stay under.", defaultTotalBytesLimit, windowBytesMax),
		DocNote: fmt.Sprintf("Cap for the whole reply (default %d). %d is the size to stay under, not a wall — this is YOUR budget for one reply, so set it as high as you are willing to read", defaultTotalBytesLimit, windowBytesMax),
		Def:     defaultTotalBytesLimit,
		DefText: strconv.Itoa(defaultTotalBytesLimit),
		Min:     1,
		// No ceiling: this is the caller's own budget for one reply.
		BoundNote: fmt.Sprintf("This is your budget for one reply, so there is no ceiling to enforce: %d bytes is the size to stay under. A reply that exceeds it cuts the remaining slots and says which ones it dropped.\n"+
			"More is another look: `read <channel> --cursor <n>`.", windowBytesMax),
	},
	{
		Name:    "on_time_overflow",
		Kind:    argEnum,
		Enum:    []string{"truncate", "fold"},
		Desc:    "truncate = show part, fold = withhold and read it yourself.",
		DocNote: "`truncate` (default) or `fold` (return no bytes this time)",
		Def:     "truncate",
		DefText: "truncate",
	},
	{
		Name:    "on_bytes_overflow",
		Kind:    argEnum,
		Enum:    []string{"truncate", "fold"},
		Desc:    "truncate = show part, fold = withhold and read it yourself.",
		DocNote: "`truncate` (default) or `fold` (return no bytes this time)",
		Def:     "truncate",
		DefText: "truncate",
	},
	{
		Name:    "on_lines_overflow",
		Kind:    argEnum,
		Enum:    []string{"truncate", "fold"},
		Desc:    "truncate = show part, fold = withhold and read it yourself.",
		DocNote: "`truncate` (default) or `fold` (return no bytes this time)",
		Def:     "truncate",
		DefText: "truncate",
	},
	{
		Name:    "learn_mode",
		Kind:    argBool,
		Desc:    "Add a 'usage' header line. Default: false.",
		DocNote: "Attach each directive's usage line once per reply",
		Def:     false,
		DefText: "false",
	},
	{
		Name: "structured_content",
		Kind: argBool,
		// OFF by default, and that is the whole point of the argument.
		//
		// A tool result has two projections and they carry the SAME bytes: the
		// rendered text in `content`, and the envelope again as `structuredContent`.
		// Sending both bills every reply twice, and a client that prints what it
		// receives shows a human the same answer in two dialects on every call. That
		// is not a corner case — it is the normal case, because the text form is what
		// a model reads most comfortably, so the duplicate is the copy almost nobody
		// consumes.
		//
		// This replaced two switches that both pointed the wrong way. `structured_only`
		// was meant to turn structuredContent off but kept the JSON and dropped the
		// text instead; `content_only` was the same intent spelled backwards, which
		// made "no duplicate" the thing you had to ask for. One positive flag with the
		// quiet default says what it does: pass true and you get the JSON twin.
		Desc:    "Also send structuredContent. Default: false.",
		DocNote: "Also send the JSON projection as `structuredContent`. **Off by default**: the two projections carry the same bytes, so the JSON copy is a duplicate a human sees on every call. Pass `true` for a client that reads structured content",
		Def:     false,
		DefText: "false",
	},
	{
		Name:    "args",
		Kind:    argStringArray,
		Desc:    "Named = positional. The value IS the shell line: put the whole command in the variable, leave only the reference in cmd. DSL and shell are never mixed.",
		DocNote: "Positional values for `$1`, `$2`, … (1-based; `$0` is refused; `${10}` is the unambiguous form for ten or more). The reference must BE the payload: the value is the shell line, and cmd is DSL, so a reference beside other shell text is refused rather than guessed at. Nothing quotes or escapes the value — if you want one word, write the quotes inside the value. EVERY value must be referenced by cmd: a value nothing names is refused before anything runs, because a dropped argument is invisible in the reply. **These values ARE recorded in the server log** — as is anything typed literally into `cmd`, since a payload or a flag value is part of the command line. A password or a token belongs in `secrets`",
		Def:     nil,
		DefText: "",
	},
	{
		Name:    "vars",
		Kind:    argMap,
		Desc:    "Named values for $NAME. Logged verbatim.",
		DocNote: "Named values for `$NAME` in `cmd`. The same mechanism as `args` under another spelling — the position decides the insertion, never which table named the value — so a `$NAME` that is the whole payload and a `$1` that is the whole payload behave identically. Every name must be referenced by cmd: an unused entry is refused before anything runs, and so is a `$NAME` this table does not define. **These values ARE recorded in the server log**, which is what makes the log a faithful record of the call: a value that must not be written down belongs in `secrets` instead",
		Def:     nil,
		DefText: "",
	},
	{
		Name:    "secrets",
		Kind:    argMap,
		Desc:    "Named values for $NAME that are NEVER logged. Put credentials here.",
		DocNote: "Named values for `$NAME` in `cmd`, expanded exactly like `vars` and refused on the same rules (unused names, undefined names, one name in both tables). The ONE difference is the record: the server log names which secrets were passed and never writes their values, while `vars` is logged verbatim. That is the whole mechanism — there is no pattern matching and no guessing, because a redactor that recognises `-p` and misses `--password` is worse than none: it reports safety it does not have. So a caller that does not want a value on disk says so by where it puts it. Note that a secret expanded into a command line is still echoed back by the remote shell, and that echo is the channel's own transcript — the terminal cannot be asked to hide what it printed",
		Def:     nil,
		DefText: "",
	},
}

// termArgDeclFor finds one argument's declaration.
func termArgDeclFor(name string) (termArgDecl, bool) {
	for _, d := range termArgDecls {
		if d.Name == name {
			return d, true
		}
	}
	return termArgDecl{}, false
}

// parseTermArgs reads the window parameters and checks them. A value outside its
// domain is an argument error, not a silent substitution: a caller that typed 0
// where it meant 8192 should be told, not quietly given a different read.
//
// IT ALSO REFUSES A MISSTyped VALUE, which is the whole reason it reads the raw
// map instead of calling the loose getters. `getFloat64(args, "timeout", 2)` cannot
// tell "absent" from "present but not a number", so `timeout: "abc"` used to
// become 2 seconds with a 200 reply — the caller's typo silently bought a different
// window. Absent still takes the table's default (that is a documented default,
// not an accident); present-and-wrong is a 400 that names the argument.
//
// This is the one place both entrances pass through: the MCP transport goes via
// handleTerm and REST calls Term directly, so a check that lived only in mcp-go's
// schema validator would cover one of the two.
//
// The defaults and bounds come from termArgDecls, so an argument added to the table
// is parsed with the default and the range the table publishes — the schema, this
// function and docs/api.md read one declaration and cannot drift.
// unknownTermArgs returns the argument names the tool does not have, in the order
// the caller sent them (sorted, since a Go map has no order and a refusal that
// reorders itself between two identical calls reads as two different mistakes).
func unknownTermArgs(args map[string]any) []string {
	var out []string
	for name := range args {
		if _, known := termArgDeclFor(name); !known {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// parseTermArgs reads the tool's arguments, refusing every value outside its domain.
func parseTermArgs(args map[string]any) (termArgs, *directive.Error) {
	w := termArgs{}
	// EVERY OUT-OF-DOMAIN VALUE IS REPORTED, IN ONE REPLY.
	//
	// A caller that sent three bad numbers used to be told about the first one, fix it,
	// resend, and be told about the second — three round trips to learn what one reply
	// could have said. The checks are collected rather than returned early, and the
	// collection is a closure so each check stays next to the value it is about.
	var problems []string
	collect := func(err *directive.Error) {
		if err != nil {
			problems = append(problems, err.Msg)
		}
	}
	// AN ARGUMENT THE TOOL DOES NOT HAVE IS REFUSED, NOT IGNORED.
	//
	// This is the exact shape the old `pipe_to` bug had: the parameter was published,
	// parsed, and then read by nobody, so a caller who asked for a pipe got its bytes
	// inline and a 200. Deleting a parameter without refusing it reproduces that from
	// the other side — the caller keeps sending it and keeps getting silence, and the
	// only way to find out is to notice the bytes came back when they should not have.
	//
	// A typo lands here too (`timeout_seconds`), which is the same failure one letter
	// away from a correct call. The names are listed from the same table the schema is
	// generated from, so the teaching cannot drift from the tool.
	if unknown := unknownTermArgs(args); len(unknown) > 0 {
		names := make([]string, 0, len(termArgDecls))
		for _, d := range termArgDecls {
			names = append(names, d.Name)
		}
		collect(&directive.Error{Msg: fmt.Sprintf("unknown argument %s; term takes %s",
			strings.Join(unknown, ", "), strings.Join(names, ", "))})
	}
	if _, given := args["timeout"]; given {
		w.TimeoutSet = true
	}
	collect(readNumberArg(args, "timeout", &w.TimeoutSeconds))
	var bytesLimit, linesLimit, totalLimit float64
	if _, given := args["bytes_limit"]; given {
		w.BytesLimitSet = true
	}
	collect(readNumberArg(args, "bytes_limit", &bytesLimit))
	collect(readNumberArg(args, "lines_limit", &linesLimit))
	collect(readNumberArg(args, "total_bytes_limit", &totalLimit))
	collect(readEnumArg(args, "on_time_overflow", &w.OnTimeOverflow))
	collect(readEnumArg(args, "on_bytes_overflow", &w.OnBytesOverflow))
	collect(readEnumArg(args, "on_lines_overflow", &w.OnLinesOverflow))
	collect(readBoolArg(args, "learn_mode", &w.LearnMode))
	if len(problems) > 0 {
		return termArgs{}, &directive.Error{Msg: strings.Join(problems, "\n")}
	}
	w.BytesLimit, w.LinesLimit, w.TotalBytesLimit = int(bytesLimit), int(linesLimit), int(totalLimit)

	// A PER-SLOT LIMIT ABOVE THE WHOLE-REPLY CAP IS NOT A LIMIT, IT IS A TYPO THAT
	// LOOKS LIKE A WISH — but only when the caller ASKED for the per-slot number.
	//
	// `bytes_limit: 50000` under a 32768 total is silently cut down to the total: the
	// caller gets fewer bytes than it named and no line saying so. That is refused.
	//
	// The default is not an ask. A caller that only says `total_bytes_limit: 64` is
	// asking for a small reply, and the default per-slot window being larger than it is
	// exactly how that is expressed — refusing it would make "cap the whole reply"
	// impossible to say without also restating a number the caller never chose.
	if _, asked := args["bytes_limit"]; asked && w.TotalBytesLimit > 0 && w.BytesLimit > w.TotalBytesLimit {
		return termArgs{}, &directive.Error{Msg: fmt.Sprintf("bytes_limit (%d) is larger than total_bytes_limit (%d), which caps the whole reply: raise total_bytes_limit too, or lower bytes_limit",
			w.BytesLimit, w.TotalBytesLimit)}
	}

	// A WINDOW NARROWER THAN ONE CHARACTER CANNOT RETURN ANYTHING, SO IT IS REFUSED
	// RATHER THAN ANSWERED WITH NOTHING.
	//
	// A byte window never ends inside a character: the bytes that would have been cut
	// stay unread, so the next read delivers them whole. That rule has no answer for a
	// window smaller than one character — it withholds the same bytes on every call and
	// the cursor never moves. The reply is a 204 with an unsatisfied range, which is
	// exactly what "there is no data" looks like, so a caller paging 1 byte at a time
	// against a stream that starts with a multi-byte character loops forever and is
	// told nothing (measured: `printf` of one U+4E2D plus four ASCII bytes, read at
	// bytes_limit 1, answered `bytes */7` on every call).
	//
	// The floor is utf8.UTFMax because that is the widest character any charset this
	// build accepts can produce, and a floor costs nothing: nobody reads 1 byte of a
	// terminal on purpose, and `0` keeps its own meaning ("the channel only").
	if w.BytesLimit > 0 && w.BytesLimit < minWindowBytes {
		return termArgs{}, &directive.Error{Msg: fmt.Sprintf("bytes_limit (%d) is smaller than one character: a window that cannot hold one whole character withholds those bytes and advances nothing, so every read answers the same empty reply. Use 0 (the channel only, no bytes) or at least %d, and page with `read <channel> --cursor <n>` instead of shrinking the window",
			w.BytesLimit, minWindowBytes)}
	}
	if err := readBoolArg(args, "structured_content", &w.StructuredContent); err != nil {
		return termArgs{}, err
	}
	vars, derr := readNamedArg(args, "vars")
	if derr != nil {
		return termArgs{}, derr
	}
	secrets, derr := readNamedArg(args, "secrets")
	if derr != nil {
		return termArgs{}, derr
	}
	named, derr := mergeNamed(vars, secrets)
	if derr != nil {
		return termArgs{}, derr
	}
	w.Vars = named
	w.Secrets = secrets
	posArgs, derr := readArgsArg(args)
	if derr != nil {
		return termArgs{}, derr
	}
	w.Args = posArgs
	return w, nil
}

// readArgsArg reads the positional values.
//
// A string is refused rather than treated as a one-element list: `args: "pwd"` and
// `args: ["pwd"]` would then be the same request spelled two ways, and a caller that
// wrote a shell-looking string would never learn that its quoting reached the server
// as data. Every element must be a string for the same reason vars values must be:
// guessing that a JSON number meant its decimal spelling would put a value in the
// substitution that the caller never sent.
func readArgsArg(args map[string]any) ([]string, *directive.Error) {
	raw, present := args["args"]
	if !present || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, &directive.Error{Msg: "args must be an array of strings, got " + typeNameOf(raw) +
			"; write it as [\"first\", \"second\"]"}
	}
	out := make([]string, len(list))
	for i, v := range list {
		s, isString := v.(string)
		if !isString {
			return nil, &directive.Error{Msg: "args[" + strconv.Itoa(i) + "] must be a string, got " + typeNameOf(v)}
		}
		out[i] = s
	}
	return out, nil
}

// readNamedArg reads one named-value dictionary: vars or secrets.
//
// The two are the same mechanism with one difference — where the values are RECORDED
// — so one function reads both and they are merged into a single substitution table.
// Keeping the reader single is what stops the two tables from drifting apart in what
// they accept: a rule added for one would otherwise have to be remembered for the
// other (issue #161 P0-1).
//
// Keys and values must both be strings: a caller that sent `{"N": 3}` meant the
// string "3", but guessing that here would mean the substitution and the caller
// disagree about what was sent. An empty or absent table is nil, which
// ParseWithSubst reads as "substitute nothing".
func readNamedArg(args map[string]any, name string) (map[string]string, *directive.Error) {
	raw, present := args[name]
	if !present || raw == nil {
		return nil, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, &directive.Error{Msg: name + " must be an object of name: value pairs, got " + typeNameOf(raw)}
	}
	out := make(map[string]string, len(obj))
	for k, v := range obj {
		if k == "" {
			return nil, &directive.Error{Msg: name + " has an entry with an empty name; every key must name the variable it fills"}
		}
		s, isString := v.(string)
		if !isString {
			return nil, &directive.Error{Msg: name + "[" + k + "] must be a string, got " + typeNameOf(v)}
		}
		out[k] = s
	}
	return out, nil
}

// mergeNamed folds secrets into the table that expands `$NAME`.
//
// ONE TABLE, TWO RECORDS. Which table a name came from changes nothing about the
// substitution — a `$NAME` is a `$NAME` — and only the log tells them apart. A name
// defined in both is refused rather than resolved by precedence: the caller wrote two
// values for one reference, and any rule picked here would be a silent choice between
// them.
func mergeNamed(vars, secrets map[string]string) (map[string]string, *directive.Error) {
	for name := range secrets {
		if _, both := vars[name]; both {
			return nil, &directive.Error{Msg: "the name " + name + " is defined in both vars and secrets; " +
				"a reference has one value, so put it in the table whose record you want"}
		}
	}
	if len(secrets) == 0 {
		return vars, nil
	}
	if len(vars) == 0 {
		return secrets, nil
	}
	out := make(map[string]string, len(vars)+len(secrets))
	for k, v := range vars {
		out[k] = v
	}
	for k, v := range secrets {
		out[k] = v
	}
	return out, nil
}

// readNumberArg fills dst with the argument's value, its declared default when the
// argument is ABSENT, or a 400 when it is present and unusable.
func readNumberArg(args map[string]any, name string, dst *float64) *directive.Error {
	d, ok := termArgDeclFor(name)
	if !ok {
		panic("no termArgDecls entry for " + name)
	}
	raw, present := args[name]
	if !present || raw == nil {
		*dst = numDefault(name)
		return nil
	}
	v, ok := asFloat(raw)
	if !ok {
		return &directive.Error{Msg: name + " must be a number, got " + typeNameOf(raw)}
	}
	if lo, ok := boundValue(d.Min); ok && v < lo {
		return &directive.Error{Msg: name + " must be >= " + numText(lo) + ", got " + numText(v) + noteSuffix(d.BoundNote)}
	}
	*dst = v
	return nil
}

// readEnumArg enforces a closed value set, and says what the set is: a caller that
// typed a plausible word needs to see the accepted ones, not just "invalid".
func readEnumArg(args map[string]any, name string, dst *string) *directive.Error {
	d, ok := termArgDeclFor(name)
	if !ok {
		panic("no termArgDecls entry for " + name)
	}
	raw, present := args[name]
	if !present || raw == nil {
		*dst = fmt.Sprint(d.Def)
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		return &directive.Error{Msg: name + " must be a string, got " + typeNameOf(raw)}
	}
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		*dst = fmt.Sprint(d.Def)
		return nil
	}
	for _, allowed := range d.Enum {
		if s == allowed {
			*dst = s
			return nil
		}
	}
	return &directive.Error{Msg: name + " must be one of " + strings.Join(d.Enum, ", ") + ", got \"" + s + "\""}
}

func readBoolArg(args map[string]any, name string, dst *bool) *directive.Error {
	d, ok := termArgDeclFor(name)
	if !ok {
		panic("no termArgDecls entry for " + name)
	}
	raw, present := args[name]
	if !present || raw == nil {
		*dst = d.Def == true
		return nil
	}
	b, ok := raw.(bool)
	if !ok {
		return &directive.Error{Msg: name + " must be true or false, got " + typeNameOf(raw)}
	}
	*dst = b
	return nil
}

// asFloat accepts every numeric shape a transport may hand over.
func asFloat(raw any) (float64, bool) {
	switch n := raw.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		v, err := n.Float64()
		return v, err == nil
	}
	return 0, false
}

// typeNameOf names what the caller actually sent, so the error is actionable.
func typeNameOf(raw any) string {
	switch raw.(type) {
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", raw)
}

// noteSuffix appends a bound's explanation, if it has one.
//
// It goes on its own line so the range stays the first thing the caller reads: the
// refusal has to work for a caller that only skims, and teach the one that reads on.
func noteSuffix(note string) string {
	if strings.TrimSpace(note) == "" {
		return ""
	}
	return "\n" + note
}

// boundValue reads a declared Min/Max bound.
func boundValue(v any) (float64, bool) {
	if v == nil {
		return 0, false
	}
	return asFloat(v)
}

// numText renders a bound or a value the way the table spells it, so an error and
// the schema do not print the same number two ways.
func numText(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// numDefault reads a numeric argument's default from the table.
//
// It panics on a missing or mistyped entry rather than falling back to a literal:
// the table is a compile-time constant set, so a mismatch is a programming error,
// and a silent fallback here would recreate exactly the drift this file removes —
// a default that exists in the parser and nowhere a caller can read it.
func numDefault(name string) float64 {
	d, ok := termArgDeclFor(name)
	if !ok {
		panic("no termArgDecls entry for " + name)
	}
	switch v := d.Def.(type) {
	case int:
		return float64(v)
	case float64:
		return v
	}
	panic("termArgDecls entry " + name + " has a non-numeric default")
}

// docTableRows renders the argument table for docs/api.md §16.
//
// This is what cmd/termdoc writes between the markers, and why the doc can no
// longer disagree with the schema about which arguments exist. Editing it by hand
// is pointless — TestTermDocTableIsGenerated fails until it is regenerated — so
// the only way to change what a caller reads is to change the table above.
func docTableRows() string {
	var b strings.Builder
	for _, d := range termArgDecls {
		note := d.DocNote
		if note == "" {
			note = d.Desc
		}
		b.WriteString("| `" + d.Name + "` | " + note + " |\n")
	}
	return b.String()
}

// The generated blocks in docs/api.md.
//
// A marker pair rather than a "replace the section" rule: the section holds prose
// that is written by hand (why the windows exist, what a slot is) and exactly one
// table that must not be. The markers say which is which, so a paragraph can be
// rewritten without the generator overwriting it and the table can be regenerated
// without the generator guessing where it ends.
const (
	termDocBegin = "<!-- BEGIN termdoc: args -->"
	termDocEnd   = "<!-- END termdoc: args -->"
)

// RenderTermDoc replaces the generated argument table in a docs/api.md body.
//
// It reports whether anything changed, so the caller can decide between a no-op
// and a rewrite (and the -check mode between success and failure) without reading
// the file twice.
//
// A missing or unpaired marker is NOT an error here: the function returns the
// input unchanged and says so, and the test that calls it asserts the markers
// exist. Reporting it as an error would make this function's contract "the doc is
// well-formed", which is a different question from "here is the rendered table".
func RenderTermDoc(doc string) (string, bool) {
	begin := strings.Index(doc, termDocBegin)
	if begin < 0 {
		return doc, false
	}
	afterBegin := begin + len(termDocBegin)
	end := strings.Index(doc[afterBegin:], termDocEnd)
	if end < 0 {
		return doc, false
	}
	end += afterBegin

	replacement := termDocBegin + "\n" + docTableRows() + termDocEnd
	current := doc[begin : end+len(termDocEnd)]
	if current == replacement {
		return doc, false
	}
	return doc[:begin] + replacement + doc[end+len(termDocEnd):], true
}
