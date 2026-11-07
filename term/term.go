// Package term is TERM/1: the protocol itself, with no runtime behind it.
//
// # The two calls you need
//
//	file, err := term.Parse("ls session; run a:1 $1")   // DSL text -> statements
//	if err != nil { ... }                               // the error teaches the shape
//	reply := term.Execute(ctx, file, myCapabilities, term.Options{})
//	fmt.Print(reply.Text())                             // a TERM/1 envelope
//
// Parse is the language. Execute is the sequencing: && and || gating, & groups
// that run together, one slot per statement, and 208 for a branch that was not
// taken. Both are protocol rules, so neither is something a caller should have to
// re-derive — getting slot alignment wrong is invisible until a caller maps slot 3
// back to the wrong statement.
//
// # The capability layer is one method
//
//	type myCapabilities struct{}
//	func (c *myCapabilities) Handle(ctx context.Context, st term.Statement) []term.Slot
//
// That is the whole obligation. st arrives parsed: Op, Target, Kind, Key, Args,
// Flags (with the three states a flag really has), Payload, Path, Cond. Return the
// slots the statement produced — a slice because a verb may address several
// objects at once, and collapsing that would break the alignment above.
//
// Handle must be safe for concurrent use: Execute runs an & group in parallel.
//
// # What is here besides those two
//
//   - the registry — Lookup, Ops, Names, and the per-verb flag and usage tables
//     that `help` and the error messages are generated from;
//   - the reply envelope — Slot and Reply rendered as an HTTP-style answer, with
//     the range and truncation rules;
//   - addresses — termcp:// locators and `<session>:<index>` handles.
//
// Nothing in this package knows what a session is, opens a socket, reads a file,
// or looks at a clock. `go list -deps ./term` names no internal package.
//
// # Where to look for a worked example
//
// The server in internal/mcp is exactly this shape: its capability handler is
// alias expansion plus the directive dispatch, and everything else it does with a
// batch comes from Execute. Read `capabilityHandler` in internal/mcp/term_exec.go
// for the two-function version of the interface above.
//
// # Stability
//
// The semantics here are the protocol's, so they follow the protocol version
// rather than the module's. Changing what a line means, what a status code means,
// or what a locator denotes is a breaking change and needs a version bump in the
// docs; adding a verb, a flag or a helper is not.
//
// The subpackages (directive, envelope, locator) are the implementation and are
// public for reading; these aliases are type aliases, not wrappers, so a value
// produced by term/directive IS a term.File and there is no conversion to drift.
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
