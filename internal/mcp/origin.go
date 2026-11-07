package mcp

import (
	"context"
	"net/http"
	"strings"

	mcpgo "github.com/open-mcp-ai/termcp/mcp"
)

// originKey carries the request's origin ("scheme://host") from the transport's
// context hook to the tools/list decoration.
type originKey struct{}

// originFromRequest builds "scheme://host" from the request the client sent: the
// host it dialed (or the one a proxy recorded after rewriting Host) and the
// scheme it reached us on. This is the address that works for whoever made the
// call — a LAN IP, a proxy name, a tunnel — and it changes with --host/--port, so
// it cannot be a startup-time constant.
//
// Both headers are client-supplied. They are used to tell a human where this
// instance is, never as a trust decision.
func originFromRequest(r *http.Request) string {
	host := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + host
}

func withRequestOrigin(ctx context.Context, r *http.Request) context.Context {
	if origin := originFromRequest(r); origin != "" {
		return context.WithValue(ctx, originKey{}, origin)
	}
	return ctx
}

// withRequestContext is what both transports install: the request's origin and
// its namespace claim, in one function. mcp-go's context hook is a single slot
// per transport (WithHTTPContextFunc replaces rather than stacks), so the two
// derivations are composed here instead of being passed one after the other —
// the second call would silently drop the first, which is a failure mode with no
// symptom until a caller asks where this instance is or which namespace it owns.
func withRequestContext(ctx context.Context, r *http.Request) context.Context {
	return withCallerNamespace(withRequestOrigin(ctx, r), r)
}

// originFor resolves the origin to advertise to this particular caller: the
// address of the request being answered, else the bind address as a last resort.
// Resolution happens per call because the right answer is "wherever this client
// reached us", which the request knows and a startup flag cannot. The fallback
// only fires for a request that names no host at all (HTTP/1.0 without Host) or
// an in-process call; it is also what a stdio bridge on loopback advertises.
func (s *Server) originFor(ctx context.Context) string {
	if o, _ := ctx.Value(originKey{}).(string); o != "" {
		return strings.TrimSuffix(o, "/")
	}
	return strings.TrimSuffix(s.baseURL, "/")
}

// decorateTools adds this instance's address to the description of the single
// entry. This is the one channel guaranteed to arrive: a tool description that
// never reaches the model means the tool can never be called, so no client can
// drop it -- unlike `instructions` (see mcpServerInstructions). It rides on
// `term` because that is the only tool there is, and it is the one whose
// `notify` directive reaches the human. The result is per-request, so the
// decoration never accumulates on the stored tool.
func (s *Server) decorateTools(ctx context.Context, result *mcpgo.ListToolsResult) {
	origin := s.originFor(ctx)
	if origin == "" {
		return
	}
	for i := range result.Tools {
		if result.Tools[i].Name != termToolName {
			continue
		}
		result.Tools[i].Description = strings.TrimSuffix(result.Tools[i].Description, " ") + " This instance's Web UI: " + origin + "/"
	}
}
