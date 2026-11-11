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
	oauth        bool
	grantID      string
	oauthClient  string
	resource     string
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
			oauth:        auth.IsOAuthCredential(ctx),
			grantID:      auth.OAuthGrantIDFromContext(ctx),
			oauthClient:  auth.OAuthClientIDFromContext(ctx),
			resource:     auth.OAuthResourceFromContext(ctx),
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
			if id != "" && !s.authorizeTransport(r.Context(), id, actor, auth.CredentialIDFromContext(r.Context()), auth.IsOAuthCredential(r.Context())) {
				http.Error(w, "MCP session belongs to a different credential", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorizeTransport(ctx context.Context, id string, actor auth.Actor, credential string, oauth bool) bool {
	owner, ok := s.transportOwners.Load(id)
	if !ok {
		return false
	}
	t := owner.(transportOwner)
	if t.principal != actor.ID {
		return false
	}
	if t.credential == credential {
		if t.oauth && (t.grantID != auth.OAuthGrantIDFromContext(ctx) ||
			t.oauthClient != auth.OAuthClientIDFromContext(ctx) ||
			t.resource != auth.OAuthResourceFromContext(ctx)) {
			return false
		}
		return true
	}
	// OAuth access tokens may rotate, but a transport session can only move to a
	// new token when the grant, client and resource remain identical. Agent
	// equality alone is insufficient because one user may own several grants.
	if !t.oauth || !oauth ||
		t.grantID == "" || t.grantID != auth.OAuthGrantIDFromContext(ctx) ||
		t.oauthClient == "" || t.oauthClient != auth.OAuthClientIDFromContext(ctx) ||
		t.resource == "" || t.resource != auth.OAuthResourceFromContext(ctx) {
		return false
	}
	t.credential = credential
	t.credentialID = strings.TrimPrefix(credential, "token:")
	s.transportOwners.Store(id, t)
	s.connections.noteOwner(id, connectionOwner{
		PrincipalID:  t.principalID,
		AgentName:    t.agentName,
		CredentialID: t.credentialID,
		Transport:    t.transport,
		ClientMode:   t.clientMode,
	})
	s.notifyConnectionsChanged()
	return true
}
