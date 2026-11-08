package mcp

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/auth"
)

type transportOwner struct {
	principal    string
	credential   string
	principalID  string
	agentName    string
	credentialID string
	transport    string
	clientMode   string
}

func (s *Server) rememberTransport(ctx context.Context, id string) connectionOwner {
	if actor, ok := auth.ActorFromContext(ctx); ok && id != "" {
		credential := auth.CredentialIDFromContext(ctx)
		credentialID := strings.TrimPrefix(credential, "token:")
		owner := transportOwner{
			principal:    actor.ID,
			credential:   credential,
			principalID:  actor.ID,
			agentName:    actor.Username,
			credentialID: credentialID,
			transport:    mcpTransportFromContext(ctx),
			clientMode:   mcpClientModeFromContext(ctx),
		}
		s.transportOwners.Store(id, owner)
		return connectionOwner{
			PrincipalID:  owner.principalID,
			AgentName:    owner.agentName,
			CredentialID: owner.credentialID,
			Transport:    owner.transport,
			ClientMode:   owner.clientMode,
		}
	}
	return connectionOwner{}
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
	if !ok {
		return false
	}
	t := owner.(transportOwner)
	return t.principal == principal && t.credential == credential
}
