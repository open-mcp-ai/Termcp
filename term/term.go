// Package term is TERM/1: the protocol itself, with no runtime behind it.
//
// # One object, two methods
//
//	t := term.New(myVerbs, term.WithHandler(myCapabilities{}))
//	file, err := t.Parse("deploy prod $1", term.Subst{Pos: []string{"make release"}})
//	if err != nil { ... }                 // the error teaches the shape
//	reply := t.Run(ctx, file)
//	fmt.Print(reply.Text())               // a TERM/1 envelope
//
// myVerbs is a *Grammar the caller built: a vocabulary is a required argument to
// New, because what a Term can parse must be exactly what its own program means.
//
// Parse is the language and Run is the sequencing: && and || gating, & groups
// that run together, one slot per statement, and 208 for a branch that was not
// taken. Both are protocol rules, so neither is something a caller should have to
// re-derive — getting slot alignment wrong is invisible until a caller maps slot 3
// back to the wrong statement.
//
// They are methods on one value rather than two package functions because the
// vocabulary a line is parsed against and the layer that answers it must be the
// same object's: passing a grammar to Parse and a handler to Run separately would
// let a caller wire a line against one table and answer it from another.
//
// # Review is the caller's, between the two calls
//
// Parse hands back the whole plan — every statement, including the branches && and
// || would skip — so a gate is a few lines where the caller already is:
//
//	file, err := t.Parse(src, sub)
//	if err != nil { return err }            // syntax is decided before anything runs
//	if !allowed(file.Statements) {          // your policy, your question, once
//		return refusal
//	}
//	reply := t.Run(ctx, file)
//
// It is not an interface here on purpose: a Reviewer would be one type with one
// implementation, and the statements are already in hand.
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
// Handle must be safe for concurrent use: Run executes an & group in parallel.
//
// # Your own verbs
//
// # Your own verbs
//
// The vocabulary is an argument, never a default: a program that embeds TERM/1
// has its own verbs, and termcp's are one table among many. An Op is only a
// declaration — how its words are cut, what its target names, which flags it
// takes — and the parser enforces what it declares:
//
//	g := term.NewGrammar([]term.Op{
//		{Name: "deploy", Kind: term.KindPayload, Target: true, Usage: "deploy <env> $1"},
//	})
//	t := term.New(g, term.WithHandler(myCaps{}))
//
// The table this build ships is not in this package. It is termcp's application
// data — its resources, its address shapes, the prose of its help pages — and it
// lives where the capability layer that answers those verbs lives, so that a
// caller reading `term` sees the language and nothing else.
//
// # What is here besides those two
//
//   - the vocabulary type — Grammar and the Op, Kind and TargetKind declarations
//     a verb is made of, which is all `help` and every error message is generated
//     from; the table itself is the caller's;
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
// batch comes from Run. Read `capabilityHandler` in internal/mcp/term_exec.go for
// the two-function version of the interface above.
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
	// Grammar is a verb vocabulary: what a parse accepts and how it cuts each
	// verb's words.
	Grammar = directive.Grammar
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

// The verb declarations a Grammar is made of. A caller that adds a verb needs
// these names: an Op is only meaningful with a Kind (how the words after the
// target are cut) and, where the target is checked, a TargetKind.
const (
	KindFlags       = directive.KindFlags
	KindPayload     = directive.KindPayload
	KindInterpreted = directive.KindInterpreted

	// PayloadVar is the PayloadType of a verb whose payload must be exactly one
	// variable reference. It is what keeps the directive language and the shell
	// apart: `run h:1 ls -la` would be read by both parsers at once.
	PayloadVar = directive.PayloadVar

	TargetAny     = directive.TargetAny
	TargetChannel = directive.TargetChannel
	TargetSession = directive.TargetSession
	TargetChain   = directive.TargetChain
)

// NewGrammar indexes a vocabulary: each verb's own name and every synonym it
// declares become ways in. The slice is copied, so the caller keeps its own.
func NewGrammar(ops []Op) *Grammar { return directive.NewGrammar(ops) }

// DefaultGrammar is the vocabulary this build ships, indexed once. A program that
// brings its own verbs does not need it; a tool that has to read a line written
// for termcp — a docs generator, a syntax checker, an embedder parsing before it
// decides — needs the vocabulary those lines were written in.
func DefaultGrammar() *Grammar { return directive.DefaultGrammar }

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
