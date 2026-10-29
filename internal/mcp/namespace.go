package mcp

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/ns"
)

// NamespaceHeader is the request header a caller uses to claim a namespace. It
// is a header rather than an initialize parameter because the transports differ:
// the streamable transport takes a per-request header on every POST, the SSE
// transport carries it once on the GET that opens the stream, and a client that
// can set one can set both. An initialize parameter would work for the first
// message and then have to be remembered per session, which is a second place
// the answer can live and disagree.
const NamespaceHeader = "X-Termcp-Namespace"

// callerNSKey carries the namespace a transport established for a caller, from
// the context hook to the guard, the list filters and the handlers.
type callerNSKey struct{}

// withCallerNamespace uses a verified Agent identity when present, otherwise
// reads the legacy claim off a request and installs it on the
// context. It is the only place the header is parsed, so every surface that
// reaches a session asks the same question of the same value.
//
// A malformed name is refused here — the request is answered 400 rather than
// silently treated as "no namespace", because a caller that mistyped its own name
// would otherwise be handed the default (shared) view, which is the opposite of
// what it asked for.
func withCallerNamespace(ctx context.Context, r *http.Request) context.Context {
	if actor, ok := auth.ActorFromContext(ctx); ok && actor.Role == auth.RoleAgent {
		return context.WithValue(ctx, callerNSKey{}, ns.ForCaller("agent-"+actor.ID))
	}
	raw := strings.TrimSpace(r.Header.Get(NamespaceHeader))
	if raw == "" {
		return ctx
	}
	if err := ns.ValidCallerName(raw); err != nil {
		// The value cannot be honoured; a blank namespace would quietly give this
		// caller the shared view. Install a sentinel that refuses every session
		// instead (see callerNamespace), so a bad claim isolates rather than
		// widens.
		return context.WithValue(ctx, callerNSKey{}, invalidNamespace)
	}
	return context.WithValue(ctx, callerNSKey{}, ns.ForCaller(raw))
}

// invalidNamespace marks a claim that could not be honoured. It is not a valid
// namespace (see ns.ValidCallerName), so it matches no resource and OwnedBy
// refuses everything.
const invalidNamespace = "ns:\x00invalid"

// callerNamespace returns the namespace this call was made under: "" for the
// ordinary (unnamespaced) surfaces — the browser, a stdio bridge, an agent that
// set no header — and the caller's own for one that claimed it.
func callerNamespace(ctx context.Context) string {
	nsValue, _ := ctx.Value(callerNSKey{}).(string)
	return nsValue
}

// callerNamespaceFor resolves the namespace a REQUEST will be answered under,
// from the context the transport built. Both HTTP transports install it (see
// register.go), and the in-process path has none, which is the unnamespaced
// default.
func (s *Server) callerNamespaceFor(ctx context.Context) string {
	return callerNamespace(ctx)
}
