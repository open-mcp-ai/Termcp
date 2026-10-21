package mcp

import (
	"encoding/json"
	"fmt"
	"math"
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

	// Min and Max are the accepted range for an argNumber. A nil bound means
	// "unbounded on that side".
	//
	// They are declared HERE, next to the default, because the three readers must
	// not be able to disagree: the schema publishes them (so a client can refuse a
	// bad value before the call), parseTermArgs enforces them (so the REST entrance,
	// which never reaches mcp-go's schema validator, refuses exactly the same
	// values), and docs/api.md renders them. Before this, the schema carried only a
	// `default`, the parser carried an if-chain of ranges, and the doc carried prose
	// — three copies of one rule.
	Min any
	Max any
}

// termArgDecls is THE table. Order is publication order: the schema and the doc
// table both read it top to bottom, so a reader scanning either one meets the
// arguments in the same sequence.
var termArgDecls = []termArgDecl{
	{
		Name:    "cmd",
		Kind:    argString,
		Desc:    "One terminal line, e.g. `run a1f3:1 ls -la`. Empty = guide.",
		DocNote: "Empty or omitted returns the guide (where am I, what can I do, what is here) — never an error. A `cmd` that is present but not a string is a 400: a command that was sent and did not survive the transport is not the same request as one never sent",
		Def:     "",
		DefText: "",
	},
	{
		Name:    "timeout",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Max seconds to wait. A pipe returns when its process exits; a pty waits the whole window. 0 = don't wait. Default: %d.", defaultTimeoutSeconds),
		DocNote: fmt.Sprintf("Seconds to wait (default %d; a pipe returns as soon as its process exits)", defaultTimeoutSeconds),
		Def:     defaultTimeoutSeconds,
		DefText: strconv.Itoa(defaultTimeoutSeconds),
		Min:     0,
		Max:     3600,
	},
	{
		Name:    "bytes_limit",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Max raw bytes; 0 = handle only. Default %d.", defaultBytesLimit),
		DocNote: fmt.Sprintf("Max bytes per slot (default %d; `0` = the handle only, no bytes)", defaultBytesLimit),
		Def:     defaultBytesLimit,
		DefText: strconv.Itoa(defaultBytesLimit),
		Min:     0,
	},
	{
		Name:    "lines_limit",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Max lines; 0 = no limit. Default %d.", defaultLinesLimit),
		DocNote: fmt.Sprintf("Max lines per slot (default %d; `0` = no line limit)", defaultLinesLimit),
		Def:     defaultLinesLimit,
		DefText: strconv.Itoa(defaultLinesLimit),
		Min:     0,
	},
	{
		Name:    "total_bytes_limit",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Absolute cap for the whole reply. Default %d.", defaultTotalBytesLimit),
		DocNote: fmt.Sprintf("Absolute cap for the whole reply (default %d)", defaultTotalBytesLimit),
		Def:     defaultTotalBytesLimit,
		DefText: strconv.Itoa(defaultTotalBytesLimit),
		Min:     1,
	},
	{
		Name:    "on_time_overflow",
		Kind:    argEnum,
		Enum:    []string{"truncate", "fold"},
		Desc:    "Time overflow: truncate = what arrived, fold = nothing.",
		DocNote: "`truncate` (default) or `fold` (return no bytes this time)",
		Def:     "truncate",
		DefText: "truncate",
	},
	{
		Name:    "on_bytes_overflow",
		Kind:    argEnum,
		Enum:    []string{"truncate", "fold"},
		Desc:    "Byte overflow: truncate = the head, fold = nothing.",
		DocNote: "`truncate` (default) or `fold` (return no bytes this time)",
		Def:     "truncate",
		DefText: "truncate",
	},
	{
		Name:    "on_lines_overflow",
		Kind:    argEnum,
		Enum:    []string{"truncate", "fold"},
		Desc:    "Line overflow: truncate = first N lines, fold = nothing.",
		DocNote: "`truncate` (default) or `fold` (return no bytes this time)",
		Def:     "truncate",
		DefText: "truncate",
	},
	{
		Name:    "pipe_to",
		Kind:    argString,
		Desc:    "Send stdout into another handle's stdin; those bytes leave the reply.",
		DocNote: "A handle whose stdin receives this reply's bytes; they leave the body",
		Def:     "",
		DefText: "",
	},
	{
		Name:    "learn_mode",
		Kind:    argBool,
		Desc:    "Attach the usage line to each directive's slot. Default: false.",
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
		Desc:    "Also send structuredContent. Default: false (both carry the same bytes).",
		DocNote: "Also send the JSON projection as `structuredContent`. **Off by default**: the two projections carry the same bytes, so the JSON copy is a duplicate a human sees on every call. Pass `true` for a client that reads structured content",
		Def:     false,
		DefText: "false",
	},
	{
		Name:    "allow_escapes",
		Kind:    argBool,
		Desc:    "Permit raw control bytes (CR, ^C, ^D, ^Z, ESC) in cmd.",
		DocNote: "Permit raw control bytes in cmd. Default: false. Pass `vars` for data (newlines/quotes) or `args` for command lines",
		Def:     false,
		DefText: "false",
	},
	{
		Name:    "args",
		Kind:    argStringArray,
		Desc:    "Values for $1, $2, … One that IS the whole payload reaches the shell as syntax; elsewhere it is one quoted word.",
		DocNote: "Positional values for `$1`, `$2`, … (1-based; `$0` is refused). A reference that is the WHOLE payload of `run`/`type` is sent to the terminal verbatim — that is how `pwd; whoami` gets to run as two commands — while a reference beside other words is substituted as one quoted word. `${10}` is the unambiguous form for ten or more",
		Def:     nil,
		DefText: "",
	},
	{
		Name:    "vars",
		Kind:    argMap,
		Desc:    "Named values for $NAME in cmd, each one quoted word.",
		DocNote: "Dictionary substituted into `cmd` at parse time ($NAME replaced by one safely quoted word). Use `args` for values that must reach the terminal as shell syntax",
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
func parseTermArgs(args map[string]any) (termArgs, *directive.Error) {
	w := termArgs{}
	if err := readNumberArg(args, "timeout", &w.TimeoutSeconds); err != nil {
		return termArgs{}, err
	}
	var bytesLimit, linesLimit, totalLimit float64
	if err := readNumberArg(args, "bytes_limit", &bytesLimit); err != nil {
		return termArgs{}, err
	}
	if err := readNumberArg(args, "lines_limit", &linesLimit); err != nil {
		return termArgs{}, err
	}
	if err := readNumberArg(args, "total_bytes_limit", &totalLimit); err != nil {
		return termArgs{}, err
	}
	w.BytesLimit, w.LinesLimit, w.TotalBytesLimit = int(bytesLimit), int(linesLimit), int(totalLimit)

	if err := readEnumArg(args, "on_time_overflow", &w.OnTimeOverflow); err != nil {
		return termArgs{}, err
	}
	if err := readEnumArg(args, "on_bytes_overflow", &w.OnBytesOverflow); err != nil {
		return termArgs{}, err
	}
	if err := readEnumArg(args, "on_lines_overflow", &w.OnLinesOverflow); err != nil {
		return termArgs{}, err
	}
	if err := readStringArg(args, "pipe_to", &w.PipeTo); err != nil {
		return termArgs{}, err
	}
	w.PipeTo = strings.TrimSpace(w.PipeTo)
	if err := readBoolArg(args, "learn_mode", &w.LearnMode); err != nil {
		return termArgs{}, err
	}
	if err := readBoolArg(args, "structured_content", &w.StructuredContent); err != nil {
		return termArgs{}, err
	}
	if err := readBoolArg(args, "allow_escapes", &w.AllowEscapes); err != nil {
		return termArgs{}, err
	}
	vars, derr := readVarsArg(args)
	if derr != nil {
		return termArgs{}, derr
	}
	w.Vars = vars
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

// readVarsArg reads the vars dictionary.
//
// Keys and values must both be strings: a caller that sent `{"N": 3}` meant the
// string "3", but guessing that here would mean the substitution and the caller
// disagree about what was sent. An empty or absent vars is nil, which
// ParseWithVars reads as "substitute nothing".
func readVarsArg(args map[string]any) (map[string]string, *directive.Error) {
	raw, present := args["vars"]
	if !present || raw == nil {
		return nil, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, &directive.Error{Msg: "vars must be an object of name: value pairs, got " + typeNameOf(raw)}
	}
	out := make(map[string]string, len(obj))
	for name, v := range obj {
		if name == "" {
			return nil, &directive.Error{Msg: "vars has an entry with an empty name; every key must name the variable it fills"}
		}
		s, isString := v.(string)
		if !isString {
			return nil, &directive.Error{Msg: "vars[" + name + "] must be a string, got " + typeNameOf(v)}
		}
		out[name] = s
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
		return &directive.Error{Msg: name + " must be >= " + numText(lo) + ", got " + numText(v)}
	}
	if hi, ok := boundValue(d.Max); ok && v > hi {
		return &directive.Error{Msg: name + " must be <= " + numText(hi) + ", got " + numText(v)}
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

func readStringArg(args map[string]any, name string, dst *string) *directive.Error {
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
	*dst = s
	return nil
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
