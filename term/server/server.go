// Package server is the transport layer of TERM/1: an HTTP entrance, the
// envelope's wire form, and the two status rules that go with them.
//
// # What this is for
//
// A program that implements TERM/1 supplies a Handler — what each verb does —
// and gets the rest of a server for free. Parsing, sequencing and slot alignment
// come from package term; this package adds the part a caller reaches over the
// network:
//
//	srv := server.New(server.Options{
//	    Term: myTerm,                      // the term.Term built over your verbs
//	    Handler: server.HandlerFunc(myHandler), // one POST, one envelope
//	})
//	http.Handle("/api/term", srv)
//
// # What is NOT here
//
// The context decorations this project's own server applies — caller namespace,
// request origin, "this write came from REST" — are hooks, not policy. They
// encode termcp's isolation and review model, which an embedder does not have
// and must not be handed by accident. Options.RequestContext is where a caller
// that does have such a model attaches it.
//
// Nothing here knows what a session is: the Handler is an interface, and this
// package never sees an implementation.
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/open-mcp-ai/termcp/term"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// Handler answers one command line.
//
// It is deliberately the narrowest useful signature — the argument object in, the
// envelope out — rather than term.Handler, which is per-statement and belongs to
// the execution layer. A transport hands over a whole request; splitting it into
// statements is the Term's job, and a transport that did it would be a second
// implementation of the language's entry point.
type Handler interface {
	// ServeTerm answers one call. args is the tool's argument object: {"cmd": …}
	// plus whatever window parameters the implementation understands.
	ServeTerm(ctx context.Context, args map[string]any) envelope.Reply
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, args map[string]any) envelope.Reply

// ServeTerm implements Handler.
func (f HandlerFunc) ServeTerm(ctx context.Context, args map[string]any) envelope.Reply {
	return f(ctx, args)
}

// Options configures the HTTP entrance.
type Options struct {
	// Term is the language, over a Handler. Required unless Params is set.
	//
	// When only a Term is given, the argument object is handed to Term.Serve, and
	// with it the protocol's own entry rules: an empty `cmd` answers the guide, a
	// syntax error answers an envelope. Those are not this package's opinions;
	// docs/protocol.md states them, and going through Serve is what makes a
	// custom server answer the same way it does.
	Term *term.Term

	// Params answers the call with the WHOLE argument object, bypassing Term.Serve.
	//
	// Use it when the implementation reads more of the tool's arguments than the
	// line does — this project's server is that case, since the window parameters
	// are its own. It is not a hook for anything else: an embedder that sets it
	// takes back the guide rule, the `cmd` type rule and the syntax-error envelope,
	// because nothing in this package gets to apply them.
	Params Handler

	// RequestContext decorates the context a request's call runs under. It is
	// where an implementation attaches what it knows about the caller — an
	// isolation namespace read off a header, the fact that the write arrived over
	// REST rather than through an agent's tool call — so that policy lives in the
	// implementation and not in this package.
	//
	// Optional. A request with no decoration is the common case for an embedder,
	// and an entrance that refused to serve without one would be demanding a model
	// it does not have.
	RequestContext func(ctx context.Context, r *http.Request) context.Context
}

// Server serves TERM/1 over HTTP.
type Server struct {
	opts Options
}

// New builds the entrance. A server with neither a capability layer nor a Term
// would have nothing to answer with, which is a programming error rather than a
// configuration one, so it says so here instead of serving 500s forever.
func New(opts Options) *Server {
	if opts.Params == nil && opts.Term == nil {
		panic("server: Options.Term or Options.Params is required")
	}
	return &Server{opts: opts}
}

// ServeHTTP answers one call.
//
// The body is the tool's argument object. A malformed body is a 400 with the same
// envelope shape as any other reply, so a caller reads one format whether it
// mistyped JSON or a directive.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	args := map[string]any{}
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		// The window parameters are numbers; the default decoding into any gives
		// float64, which is what a term argument reader expects. An empty body is
		// legal and means the same thing as an empty call: the guide.
		if err := dec.Decode(&args); err != nil && err.Error() != "EOF" {
			WriteEnvelope(w, http.StatusBadRequest, envelope.Reply{Slots: []envelope.Slot{{
				Code:    http.StatusBadRequest,
				Headers: map[string]string{"Error-Code": "invalid_argument"},
				Body:    "request body must be a JSON object of term arguments: " + err.Error(),
			}}})
			return
		}
	}
	ctx := r.Context()
	if s.opts.RequestContext != nil {
		ctx = s.opts.RequestContext(ctx, r)
	}
	// TWO ROUTES, and the difference is which one owns the argument object.
	//
	// An implementation that has window parameters, structured content or its own
	// `args` vocabulary reads the whole map: that is capability-layer work, so
	// Options.Params is the only way in and this package stays out of it.
	//
	// One that has neither gives only the line, and then the empty-calls-guide rule
	// is the PROTOCOL'S rather than a convention this server picked — so it goes
	// through Term.Serve and an embedder cannot get it wrong by hand.
	var reply envelope.Reply
	switch {
	case s.opts.Params != nil:
		reply = s.opts.Params.ServeTerm(ctx, args)
	case s.opts.Term != nil:
		reply = s.opts.Term.Serve(ctx, args)
	default:
		// Options.Term or Options.Params: one is required, and New says so.
		// Reaching here means a Term was supplied without a Handler.
		reply = envelope.Reply{Slots: []envelope.Slot{{
			Code:    403,
			Headers: map[string]string{"Error-Code": "no_capability_layer"},
			Body:    "not run: no capability layer is configured",
		}}}
	}
	WriteEnvelope(w, ReplyStatus(reply), reply)
}

// ReplyStatus is the HTTP status of a reply: the REQUEST succeeded if any slot
// did.
//
// A single failing slot is a 200 with a 4xx inside it, which is what "the status
// code answers for the request" means — but a reply where every slot failed has
// nothing that succeeded, so it is reported as its first slot's code. That keeps
// `curl -f` and a naive HTTP client useful without making a batch's partial
// success look like a failure.
func ReplyStatus(reply envelope.Reply) int {
	if len(reply.Slots) == 0 {
		return http.StatusNoContent
	}
	for _, slot := range reply.Slots {
		if slot.Code < 400 {
			return http.StatusOK
		}
	}
	return reply.Slots[0].Code
}

// WriteEnvelope serves one reply.
//
// The body is the tool's structured projection, SLOT FOR SLOT: one slot arrives as
// an object, several arrive wrapped in {"slots": [...]}. A bare array for the batch
// case would be the friendlier REST shape and is exactly what this entrance must
// not invent — a caller that learns one shape here and another from the tool has
// two descriptions of one protocol, and only one of them is true. The wrapper is
// also the honest one: a reply is a set of slots, not a list.
//
// The rendered form rides along as X-Termcp-Text, which keeps the shape a model
// reads available to a human debugging a curl call instead of making them decode
// JSON to read a status line. It is a convenience, not the contract: the body is,
// and a header cannot carry newlines, so the rendering is escaped.
func WriteEnvelope(w http.ResponseWriter, status int, reply envelope.Reply) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// `content:false` declines the rendered projection. Over REST the body is the
	// JSON one and this header is the rendering, so the same flag drops the header
	// rather than emptying the reply — one rule, both entrances.
	if !reply.NoText {
		if text := reply.Text(); text != "" {
			w.Header().Set("X-Termcp-Text", EscapeHeader(text))
		}
	}
	w.WriteHeader(status)
	structured := reply.Structured()
	var body any
	switch len(structured) {
	case 0:
		body = map[string]any{"slots": []any{}}
	case 1:
		body = structured[0]
	default:
		body = map[string]any{"slots": structured}
	}
	_ = json.NewEncoder(w).Encode(body)
}

// EscapeHeader renders a multi-line rendering as a single header value.
//
// Newlines become literal "\n" so the value survives the header grammar, which is
// the same reason the envelope renders them at all: the text is for a reader, and a
// reader that cannot see the line structure is looking at an unreadable run of
// header stamps.
//
// CARRIAGE RETURNS ARE ESCAPED, NOT DROPPED. The previous version fell through to
// `default` for '\r' and appended nothing, so the same field rendered two different
// strings depending on the projection: a Path holding a CR kept it in the JSON body
// and silently lost it in X-Termcp-Text. A caller comparing the two — or reading
// only the header — then had a path that does not exist, with nothing to indicate a
// byte had been removed. Escaping keeps the two projections byte-identical, which
// is the whole reason they are generated from one value.
func EscapeHeader(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// keep the term import honest: Term is the type the entrance is built over, and
// the line's entry rules live behind Term.Serve.
var _ = term.File{}
