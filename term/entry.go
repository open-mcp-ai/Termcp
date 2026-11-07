package term

import (
	"context"

	"github.com/open-mcp-ai/termcp/term/directive"
)

// A Term is the language, configured once: the vocabulary it parses against and
// the capability layer that answers each verb.
//
// It is the whole public surface. A program that embeds TERM/1 builds one of
// these and calls Parse and Run; there is no second way in, so the vocabulary a
// line is parsed against and the layer that answers it cannot be wired
// inconsistently.
//
//	t := term.New(myVerbs, term.WithHandler(myCaps))
//	file, err := t.Parse("deploy prod $1", term.Subst{Pos: []string{"make"}})
//	reply := t.Run(ctx, file)
//
// The vocabulary is a required argument, not a default: a program that embeds
// TERM/1 has its own verbs, and there is no built-in table in this package to
// fall back to. What a Term can parse is exactly what its Grammar declares.
type Term struct {
	grammar *Grammar
	handler Handler
	opts    Options
}

// Option configures a Term at construction.
type Option func(*Term)

// WithHandler sets the capability layer: what each verb does.
func WithHandler(h Handler) Option { return func(t *Term) { t.handler = h } }

// WithOptions sets the evaluation's own knobs.
func WithOptions(o Options) Option { return func(t *Term) { t.opts = o } }

// New builds a Term over a vocabulary.
//
// The grammar is required because it IS the language: a Term with no verbs can
// parse nothing, and defaulting to some table would make a foreign program's DSL
// answer to verbs it never declared.
//
// A handler is optional: without one a Term still parses, which is what a caller
// that only wants the syntax check uses. Run then refuses rather than pretending.
func New(g *Grammar, opts ...Option) *Term {
	t := &Term{grammar: g}
	for _, o := range opts {
		if o != nil {
			o(t)
		}
	}
	return t
}

// Grammar is the vocabulary this Term parses against.
func (t *Term) Grammar() *Grammar { return t.grammar }

// Parse turns one line into statements, or into a syntax error that teaches the
// working shape. It does not evaluate: the caller decides what each verb does.
//
// sub fills the language's variable holes before parsing, so the values the
// handler would see are the values that were parsed.
func (t *Term) Parse(src string, sub Subst) (*File, error) {
	return directive.ParseGrammar(src, sub, t.grammar)
}

// Run executes a parsed line, handing each statement to the capability layer.
//
// REVIEW IS THE CALLER'S, between Parse and Run: the plan is already in hand, so
// a gate is a few lines at the call site rather than an interface here. This method
// does not know a review exists.
//
// A Term built without a handler refuses to run rather than answering as if it
// had: parsing is still useful on its own, pretending is not.
func (t *Term) Run(ctx context.Context, file *File) Reply {
	if file == nil || len(file.Statements) == 0 {
		return Reply{}
	}
	if t.handler == nil {
		return refused(file, "no capability layer is configured")
	}
	return executeGrammar(ctx, file, t.handler, t.opts, t.grammar)
}

// refused is the reply for a Term that was asked to run with no capability layer.
//
// ONE SLOT, not one per statement: nothing ran, so per-statement codes would
// invent an outcome for work that never started.
func refused(file *File, reason string) Reply {
	path := ""
	if len(file.Statements) > 0 {
		path = file.Statements[0].Path
	}
	return Reply{Slots: []Slot{{
		Code:    403,
		Headers: map[string]string{"Path": path, "Error-Code": "no_capability_layer"},
		Body:    "not run: " + reason + "\n",
	}}}
}
