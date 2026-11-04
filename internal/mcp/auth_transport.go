package mcp

import (
	"context"
	"net/http"

	"github.com/open-mcp-ai/termcp/internal/auth"
)

type transportOwner struct{ principal, credential string }

func (s *Server) rememberTransport(ctx context.Context, id string) {
	if actor, ok := auth.ActorFromContext(ctx); ok && id != "" {
		s.transportOwners.Store(id, transportOwner{actor.ID, auth.CredentialIDFromContext(ctx)})
	}
}

// authenticatedTransport prevents a valid credential from adopting another
// credential's MCP transport session.
func (s *Server) authenticatedTransport(next http.Handler, sse bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if actor, ok := auth.ActorFromContext(r.Context()); ok {
			id := r.Header.Get("Mcp-Session-Id")
			if sse {
				id = r.URL.Query().Get("sessionId")
			}
			if len(r.Header.Values("Mcp-Session-Id")) > 1 || sse && len(r.URL.Query()["sessionId"]) > 1 {
				http.Error(w, "ambiguous MCP session", http.StatusBadRequest)
				return
			}
			if id != "" && !s.transportOwnedBy(id, actor.ID, auth.CredentialIDFromContext(r.Context())) {
				http.Error(w, "MCP session belongs to a different credential", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) transportOwnedBy(id, principal, credential string) bool {
	owner, ok := s.transportOwners.Load(id)
	return ok && owner.(transportOwner) == (transportOwner{principal: principal, credential: credential})
}
