package term

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Serve is the protocol's own entry point: one argument object in, one envelope
// out.
//
// It is the step between "a transport handed us a request" and "a line is
// running", and it is the protocol's rather than a server's for a reason that is
// written in docs/protocol.md: an empty `cmd` means the guide, and a syntax error
// is an envelope rather than a transport failure. Both are rules about what a
// caller receives, so an implementation that had to re-derive them would be
// implementing TERM/1 from the document — which is exactly what this package
// exists to make unnecessary.
//
// # What it does
//
//	cmd, present, err := args["cmd"]     a non-string is a 400, not a guide
//	empty (or absent)                    → Guide()
//	otherwise                            → Parse, then Run
//	a syntax error                       → a 400 slot carrying it
//
// # What it deliberately does not do
//
// It does not read window parameters. `bytes_limit`, `lines_limit` and the rest
// are the tool's arguments, and cutting a reply is a decision about what a caller
// reads rather than about what a line means — an implementation that has no such
// budget simply does not apply one. Leaving them unread here keeps this method
// honest about the one thing it is: the line's entry point.
//
// # Who calls it, and who does not
//
// THIS PROJECT'S OWN REST AND MCP ENTRANCES DO NOT. They go through
// internal/mcp.executeTerm, which adds alias expansion, dry runs, the window
// parameters and structured content — capability-layer work that has no place in a
// protocol package. The overlap between the two is the guide and the syntax-error
// envelope, and that overlap is deliberate rather than an unfinished merge: an
// embedder that has no aliases and no windows should not have to re-derive those
// two answers, and one that has them must apply its own machinery around this call.
//
// So when these two drift apart, the question is not "why not one function" but
// "which of the two rules was supposed to be shared". The answer is the two above.
//
// It does not apply review, admission or isolation. Those are decisions about a
// capability, and they belong to the Handler (or to a RequestContext hook on the
// transport), not to the language.
func (t *Term) Serve(ctx context.Context, args map[string]any) Reply {
	line, present, problem := cmdArg(args)
	if problem != "" {
		return Reply{Slots: []Slot{{
			Code:    400,
			Headers: map[string]string{"Error-Code": "invalid_argument"},
			Body:    problem,
		}}}
	}
	if !present || line == "" {
		// AN EMPTY CALL IS LEGAL AND IS THE GUIDE. It is the first move of every
		// recovery: a caller that has just been woken up knows nothing, so this has
		// to be cheap and never an error.
		return t.Guide()
	}
	file, err := t.Parse(line, Subst{})
	if err != nil {
		reply := SyntaxReply(err)
		// A CALLER WHO MISSED THE VERB NEEDS THE VERBS. termcp's own entrance answers
		// an unknown directive with the whole table, and an embedder's should answer
		// with its own — that is the same help, not a copy of this project's list,
		// which would name verbs the server does not have.
		if len(reply.Slots) == 1 && reply.Slots[0].Headers["Error-Code"] == "unknown_directive" {
			reply.Slots[0].Headers["Usage"] = t.verbList()
		}
		return reply
	}
	return t.Run(ctx, file)
}

// Guide is the answer to an empty call: where am I and what can I do.
//
// It is generated from the Grammar, so a program that declares its own verbs gets
// its own guide — including one that shares no verb with this project's.
func (t *Term) Guide() Reply {
	if t.guide != nil {
		return t.guide()
	}
	var b []byte
	// "TERM/1", not "the termcp directive language": the protocol is what this
	// endpoint speaks, and the vocabulary below belongs to whoever built it.
	b = append(b, "TERM/1 — one command line in, one envelope of slots out.\n\n"...)
	b = append(b, "Send {\"cmd\": \"<line>\"} to run one. The verbs this server declares:\n\n"...)
	hasHelp := false
	for _, op := range t.grammar.Ops() {
		if op.Name == "help" {
			hasHelp = true
		}
		// Usage is the one line the vocabulary declares; an Op built without it
		// still has to be listed, or the verb is invisible to the reader.
		line := op.Usage
		if line == "" {
			line = op.Name
		}
		b = append(b, line...)
		b = append(b, '\n')
	}
	// THE POINTED-AT VERB HAS TO EXIST. This sentence used to be unconditional, so
	// a server whose vocabulary has no `help` — which is the common case for an
	// embedder — ended its own guide by telling the caller to run a directive that
	// would come back "unknown directive". A guide is the first move of every
	// recovery; one that sends a lost caller into a second error is worse than no
	// guide.
	if hasHelp {
		b = append(b, "\n`help <verb>` explains one; `help` lists them all with their flags."...)
	}
	return Reply{Slots: []Slot{{Code: 200, Body: string(b)}}}
}

// SyntaxReply turns a parse error into the slot a caller reads, and is exported
// because a server that keeps its own entrance still has to answer a syntax error
// the same way this one does.
//
// THE ERROR CODE IS `invalid_argument`, NOT `syntax_error`. This function used to
// invent the latter, which meant the two entrances of one protocol disagreed about
// the same line: termcp's own REST and MCP surface answer `invalid_argument` (twelve
// tests pin it, and it is what a caller switching on the code expects), while an
// embedder built on this package answered a name that appears nowhere else in the
// project or its documentation. A second vocabulary for one situation is worse than
// no vocabulary: a caller has to guess which server it is talking to.
//
// The usage line rides along when the parser knew one, because a refusal that only
// says "no" makes the caller guess at the shape that would have worked.
func SyntaxReply(err error) Reply {
	slot := Slot{
		Code:    400,
		Headers: map[string]string{"Error-Code": ParseErrorCode(nil, err)},
		Body:    err.Error(),
	}
	var derr *Error
	if errors.As(err, &derr) && derr.Usage != "" {
		slot.Headers["Usage"] = derr.Usage
	}
	return Reply{Slots: []Slot{slot}}
}

// ParseErrorCode names a parse failure for a caller that branches on the answer.
//
// TWO NAMES, NOT ONE, AND THE SPLIT MATTERS TO A CALLER: a line whose first word is
// not a verb it knows needs a different shape, while a line that is malformed in
// its arguments needs a different argument. Both are 400 — the status was never
// where the distinction lived.
//
// IT LIVES HERE SO THE TWO ENTRANCES CANNOT ANSWER DIFFERENTLY. It used to be
// `internal/mcp`'s private classification, which meant an embedder's entrance called
// every parse failure `invalid_argument` — including "unknown directive", which is
// the one case with an obvious remedy (list the verbs). A test comparing the two
// faces on one input each is what surfaced it.
//
// A nil Grammar means the DefaultGrammar, matching Grammar.Lookup's own rule; that
// is what a caller with no vocabulary of its own is asking about.
func ParseErrorCode(g *Grammar, err error) string {
	var derr *Error
	if !errors.As(err, &derr) || derr.Op == "" {
		return "invalid_argument"
	}
	if _, known := g.Lookup(derr.Op); !known {
		return "unknown_directive"
	}
	return "invalid_argument"
}

// verbList is the one-line index of this vocabulary, for the reply that has to say
// "that is not a verb you can use here".
func (t *Term) verbList() string {
	names := make([]string, 0, 16)
	for _, op := range t.grammar.Ops() {
		names = append(names, op.Name)
	}
	return strings.Join(names, " | ")
}

// cmdArg reads the command line out of a tool's arguments.
//
// A non-string `cmd` is refused rather than folded into "absent". The two used to
// be the same answer — a loose getter turned `{"cmd": 42}` into the guide — which
// meant a caller that sent the wrong type was told what to do next instead of what
// it did wrong, and could not tell the two apart.
func cmdArg(args map[string]any) (line string, present bool, problem string) {
	raw, ok := args["cmd"]
	if !ok || raw == nil {
		return "", false, ""
	}
	s, isString := raw.(string)
	if !isString {
		return "", false, fmt.Sprintf("cmd must be a string, got %T; send the command line as a string, or omit cmd entirely to read the guide", raw)
	}
	return s, true, ""
}
