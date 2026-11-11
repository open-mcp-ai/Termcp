package auth

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/oauthserver"
)

type oauthUserView struct {
	loginUser
	GrantCount   int    `json:"grant_count"`
	ActiveGrants int    `json:"active_grant_count"`
	AgentCount   int    `json:"agent_count"`
	AvatarPath   string `json:"avatar_path,omitempty"`
}

type oauthGrantView struct {
	oauthGrant
	UserDisplayName string `json:"user_display_name"`
	UserUsername    string `json:"user_username,omitempty"`
	AgentName       string `json:"agent_name"`
	ClientName      string `json:"client_name"`
	Status          string `json:"status"`
	ActiveAccess    int    `json:"active_access_tokens"`
	ActiveRefresh   int    `json:"active_refresh_tokens"`
}

func (m *Managed) oauthUsers(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/auth/oauth/users" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		m.mu.Lock()
		state := cloneManagedState(m.state)
		m.mu.Unlock()
		now := m.now().UTC()
		users := make([]oauthUserView, 0, len(state.LoginUsers))
		for _, user := range state.LoginUsers {
			view := oauthUserView{loginUser: user}
			if user.AvatarFile != "" {
				view.AvatarPath = state.OAuthServer.PublicURL + "/auth/oauth/avatar/" + user.AvatarFile
			}
			for _, grant := range state.OAuthGrants {
				if grant.UserID != user.ID {
					continue
				}
				view.GrantCount++
				if grantActive(grant, now) {
					view.ActiveGrants++
				}
			}
			for _, agent := range state.Agents {
				if agent.OwnerUserID == user.ID {
					view.AgentCount++
				}
			}
			users = append(users, view)
		}
		sort.Slice(users, func(i, j int) bool {
			return strings.ToLower(users[i].DisplayName) < strings.ToLower(users[j].DisplayName)
		})
		authJSON(w, http.StatusOK, map[string]any{"users": users})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/auth/oauth/users/")
	if id == "" || strings.Contains(id, "/") || r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Enabled  *bool  `json:"enabled"`
		Revision uint64 `json:"revision"`
	}
	if err := decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input); err != nil || input.Enabled == nil {
		authError(w, http.StatusBadRequest, "enabled and revision are required")
		return
	}
	var cancel map[string]string
	err := m.commitState(r.Context(), func(next *managedState) (func(), error) {
		user, ok := next.LoginUsers[id]
		if !ok {
			return nil, &statusError{status: http.StatusNotFound, err: errors.New("login user not found")}
		}
		if user.Revision != input.Revision {
			return nil, &statusError{status: http.StatusConflict, err: errors.New("login user changed; reload before saving")}
		}
		user.Enabled = *input.Enabled
		user.Revision++
		user.UpdatedAt = m.now().UTC()
		next.LoginUsers[id] = user
		cancel = map[string]string{}
		if !*input.Enabled {
			now := m.now().UTC()
			for grantID, grant := range next.OAuthGrants {
				if grant.UserID != id {
					continue
				}
				if grant.RevokedAt == nil {
					grant.RevokedAt = &now
					grant.RevokeReason = "login_user_disabled"
					next.OAuthGrants[grantID] = grant
				}
				for signature, access := range next.OAuthAccess {
					if access.GrantID == grantID && access.RevokedAt == nil {
						access.RevokedAt = &now
						next.OAuthAccess[signature] = access
						cancel[access.ID] = grant.AgentID
					}
				}
				for signature, refresh := range next.OAuthRefresh {
					if refresh.GrantID == grantID && refresh.RevokedAt == nil {
						refresh.RevokedAt = &now
						next.OAuthRefresh[signature] = refresh
					}
				}
			}
			for signature := range next.OAuthCodes {
				pending, err := pendingGrantFromStored(next.OAuthCodes[signature].Request)
				if err == nil && pending.UserID == id {
					delete(next.OAuthCodes, signature)
				}
			}
		}
		next.OAuthRevision++
		return func() { m.cancelOAuthCredentials(cancel) }, nil
	})
	if err != nil {
		if status, ok := err.(*statusError); ok {
			authError(w, status.status, status.err.Error())
		} else {
			authError(w, http.StatusInternalServerError, "could not update login user")
		}
		return
	}
	authJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (m *Managed) oauthGrants(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/auth/oauth/grants" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		m.mu.Lock()
		state := cloneManagedState(m.state)
		m.mu.Unlock()
		now := m.now().UTC()
		grants := make([]oauthGrantView, 0, len(state.OAuthGrants))
		for _, grant := range state.OAuthGrants {
			user := state.LoginUsers[grant.UserID]
			agent := state.Agents[grant.AgentID]
			client := state.OAuthClients[grant.ClientID]
			view := oauthGrantView{
				oauthGrant:      grant,
				UserDisplayName: user.DisplayName,
				UserUsername:    user.Username,
				AgentName:       agent.Name,
				ClientName:      client.Name,
				Status:          grantStatus(grant, user, agent, client, now),
			}
			for _, record := range state.OAuthAccess {
				if record.GrantID == grant.ID && record.RevokedAt == nil && now.Before(record.ExpiresAt) {
					view.ActiveAccess++
				}
			}
			for _, record := range state.OAuthRefresh {
				if record.GrantID == grant.ID && record.RevokedAt == nil && now.Before(record.ExpiresAt) {
					view.ActiveRefresh++
				}
			}
			grants = append(grants, view)
		}
		sort.Slice(grants, func(i, j int) bool { return grants[i].CreatedAt.After(grants[j].CreatedAt) })
		authJSON(w, http.StatusOK, map[string]any{"grants": grants})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/auth/oauth/grants/")
	if id == "" || strings.Contains(id, "/") || r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var cancel map[string]string
	err := m.commitState(r.Context(), func(next *managedState) (func(), error) {
		grant, ok := next.OAuthGrants[id]
		if !ok {
			return nil, &statusError{status: http.StatusNotFound, err: errors.New("OAuth grant not found")}
		}
		now := m.now().UTC()
		if grant.RevokedAt == nil {
			grant.RevokedAt = &now
			grant.RevokeReason = "admin_revoked"
			next.OAuthGrants[id] = grant
		}
		cancel = map[string]string{}
		for signature, access := range next.OAuthAccess {
			if access.GrantID == id && access.RevokedAt == nil {
				access.RevokedAt = &now
				next.OAuthAccess[signature] = access
				cancel[access.ID] = grant.AgentID
			}
		}
		for signature, refresh := range next.OAuthRefresh {
			if refresh.GrantID == id && refresh.RevokedAt == nil {
				refresh.RevokedAt = &now
				next.OAuthRefresh[signature] = refresh
			}
		}
		next.OAuthRevision++
		return func() { m.cancelOAuthCredentials(cancel) }, nil
	})
	if err != nil {
		if status, ok := err.(*statusError); ok {
			authError(w, status.status, status.err.Error())
		} else {
			authError(w, http.StatusInternalServerError, "could not revoke OAuth grant")
		}
		return
	}
	authJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func grantStatus(grant oauthGrant, user loginUser, agent agentRecord, client oauthserver.Client, now time.Time) string {
	switch {
	case grant.RevokedAt != nil:
		return "revoked"
	case !now.Before(grant.ExpiresAt):
		return "expired"
	case !user.Enabled:
		return "user_disabled"
	case !agent.Enabled:
		return "agent_disabled"
	case !client.Enabled:
		return "client_disabled"
	default:
		return "active"
	}
}
