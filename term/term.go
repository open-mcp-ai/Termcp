// Package term is TERM/1: the protocol itself, with no runtime behind it.
//
// Three things live here, and they are deliberately the only three:
//
//   - the directive language — parse a `cmd` line into a *File, and the
//     registry that says which verbs, targets and flags exist;
//   - the reply envelope — a Slot/Reply rendered as an HTTP-style answer
//     (status line, headers, body), including the range and truncation rules;
//   - addresses — termcp:// locators and `<session>:<index>` handles.
//
// Nothing in this package knows what a session is, opens a socket, reads a
// file, or looks at a clock. Given a line it returns an AST or a syntax
// error; given an AST and a handler supplied by the caller it evaluates in
// order. That is what makes it reusable: an implementation of TERM/1 does not
// have to be this project's server, and this project's server does not have to
// be the only thing that parses it.
//
// # For callers who want one import path
//
// The implementation is split three ways (directive, envelope, locator) so each
// can be read on its own; this file renames the useful parts so a caller can
// write term.Parse and term.Slot without choosing between them. Both spellings
// are public and neither is going away — these are type aliases, not wrappers,
// so a value produced by term/directive is a term.File.
//
// # Stability
//
// The semantics here are the protocol's, so they follow the protocol version
// rather than the module's. Changing what a line means, what a status code
// means, or what a locator denotes is a breaking change and needs a version
// bump in the docs; adding a verb, a flag or a helper is not.
package term

import (
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
	"github.com/open-mcp-ai/termcp/term/locator"
)

// The language.
type (
	// File is one `cmd` line parsed: statements in the order they were written.
	File = directive.File
	// Statement is one verb with its target, flags and value.
	Statement = directive.Statement
	// Op is one entry of the directive registry: its verb, kinds, flags and
	// the usage text both `help` and the error messages are generated from.
	Op = directive.Op
	// Kind says what a verb does (core, file, channel, …).
	Kind = directive.Kind
	// TargetKind says what a verb's target names.
	TargetKind = directive.TargetKind
	// FlagValue says whether a flag takes a value, and what shape it takes.
	FlagValue = directive.FlagValue
	// Error is a syntax error with the position it was found at.
	Error = directive.Error
	// Subst fills the language's variable holes before parsing.
	Subst = directive.Subst
)

// Parse turns one line into statements, or into a syntax error that teaches the
// working shape. It does not evaluate: this is a parser, and the caller decides
// what each verb does.
//
// This is the protocol's front door. ParseLocator is the address reader beside
// it, kept under a different name because two functions called Parse in one
// package would be a coin flip at every call site.
func Parse(src string) (*File, error) { return directive.Parse(src) }

// ParseWithSubst is Parse with the variable holes filled first.
func ParseWithSubst(src string, sub Subst) (*File, error) {
	return directive.ParseWithSubst(src, sub)
}

// Lookup finds a verb in the registry; Ops lists them all; Names lists their
// names. `help` is generated from the same table these read.
func Lookup(name string) (Op, bool) { return directive.Lookup(name) }
func Ops() []Op                     { return directive.Ops() }
func Names() []string               { return directive.Names() }

// FlagSummary, HasSummaries and SummaryExample answer what a flag means,
// straight from the registry, so the help text cannot drift from the parser.
func FlagSummary(op, flag string) (string, bool)       { return directive.FlagSummary(op, flag) }
func HasSummaries(op string) bool                      { return directive.HasSummaries(op) }
func SummaryExample(op string) (string, bool)          { return directive.SummaryExample(op) }
func NodeNote(name string) (string, bool)              { return directive.NodeNote(name) }
func DetectRawControl(src string) (byte, string, bool) { return directive.DetectRawControl(src) }
func SplitArgv(raw string) (string, []string, error)   { return directive.SplitArgv(raw) }
func SessionTargetProblem(op Op, target string) string {
	return directive.SessionTargetProblem(op, target)
}
func ChannelTargetProblem(op Op, target string) string {
	return directive.ChannelTargetProblem(op, target)
}
func ChannelIndex(word string) (string, bool) { return directive.ChannelIndex(word) }
func NamesAChannel(target string) bool        { return directive.NamesAChannel(target) }

// The reply envelope.
type (
	// Slot is one answer: a status code, headers and a body.
	Slot = envelope.Slot
	// Reply is a batch of slots, i.e. one `term` call's whole answer.
	Reply = envelope.Reply
)

// ContentRange and UnsatisfiedRange render the range headers; RangeEnd reads one.
func ContentRange(start, end, total int64) string { return envelope.ContentRange(start, end, total) }
func UnsatisfiedRange(total int64) string         { return envelope.UnsatisfiedRange(total) }
func RangeEnd(v string) (int64, bool)             { return envelope.RangeEnd(v) }

// Addresses.
type (
	// Parsed is a parsed termcp:// locator.
	Parsed = locator.Parsed
	// Handle is a `<session>:<index>` channel handle split into its parts.
	Handle = locator.Handle
)

// ParseLocator reads a termcp:// locator. LooksLike answers whether a string is
// one at all; SplitHandle splits a `<session>:<index>` channel handle.
func ParseLocator(raw string) (*Parsed, error) { return locator.Parse(raw) }
func LooksLike(s string) bool                  { return locator.LooksLike(s) }
func SplitHandle(s string) Handle              { return locator.SplitHandle(s) }
