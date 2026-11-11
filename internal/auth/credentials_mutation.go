package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

type oauthCancellation struct {
	principal  string
	credential string
}

func (m *Managed) credentialMutation(w http.ResponseWriter, r *http.Request) {
	var result any
	targetID := auditTargetID(r.URL.Path)
	beforeAccessRevision := m.accessRevision()
	var canceledTokens []string
	var canceledOAuth []oauthCancellation
	var affectedPrincipals []string
	var accessRevisionChanged bool
	err := m.commitState(r.Context(), func(s *managedState) (func(), error) {
		fail := func(status int, message string) error {
			return &statusError{status: status, err: errors.New(message)}
		}
		switch {
		case r.URL.Path == "/auth/principals" && r.Method == http.MethodPost:
			var input struct {
				Name       string `json:"name"`
				GroupID    string `json:"group_id"`
				IssueToken bool   `json:"issue_token"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil || !validLabel(input.Name) {
				return nil, fail(http.StatusBadRequest, "a valid principal name and group binding are required")
			}
			if len(s.Agents) >= maxAgents {
				return nil, fail(http.StatusConflict, "principal limit reached")
			}
			if input.GroupID != "" {
				group, ok := s.Groups[input.GroupID]
				if !ok || !group.Enabled {
					return nil, fail(http.StatusBadRequest, "permission group is missing or disabled")
				}
			}
			id, err := randomSecret()
			if err != nil {
				return nil, err
			}
			agent := agentRecord{ID: id, Name: input.Name, GroupID: input.GroupID, Enabled: true, Revision: 1, CreatedAt: m.now().UTC(), Origin: agentOriginManual}
			s.Agents[id] = agent
			targetID = agent.ID
			result = map[string]any{"principal": agent}
			if input.IssueToken {
				token, raw, err := m.issueToken(s, id, input.Name, nil)
				if err != nil {
					return nil, fail(http.StatusBadRequest, err.Error())
				}
				result = map[string]any{"principal": agent, "token": raw, "credential": tokenInfoOf(token, m.now().UTC())}
				auditDetail(r.Context(), func(draft *auditDraft) { draft.action = "principal.create_with_credential" })
			}
		case r.URL.Path == "/auth/tokens" && r.Method == http.MethodPost:
			var input struct {
				PrincipalID string     `json:"principal_id"`
				Label       string     `json:"label"`
				ExpiresAt   *time.Time `json:"expires_at"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 262144), &input) != nil {
				return nil, fail(http.StatusBadRequest, "invalid token request")
			}
			principal := input.PrincipalID
			if principal == "" {
				principal = s.Admin.ID
			}
			token, raw, err := m.issueToken(s, principal, input.Label, input.ExpiresAt)
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, errActiveCredential) {
					status = http.StatusConflict
				}
				return nil, fail(status, err.Error())
			}
			result = map[string]any{"token": raw, "credential": tokenInfoOf(token, m.now().UTC())}
			targetID = token.ID
		case strings.HasPrefix(r.URL.Path, "/auth/tokens/") && strings.HasSuffix(r.URL.Path, "/rotate") && r.Method == http.MethodPost:
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/auth/tokens/"), "/rotate")
			if id == "" || strings.Contains(id, "/") {
				return nil, fail(http.StatusNotFound, "active token not found")
			}
			var input struct {
				Label     string     `json:"label"`
				ExpiresAt *time.Time `json:"expires_at"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil {
				return nil, fail(http.StatusBadRequest, "invalid token rotation request")
			}
			previous, next, raw, err := m.rotateToken(s, id, input.Label, input.ExpiresAt)
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, errNoActiveCredential) {
					status = http.StatusNotFound
				}
				return nil, fail(status, err.Error())
			}
			canceledTokens = append(canceledTokens, previous.ID)
			result = map[string]any{"token": raw, "credential": tokenInfoOf(next, m.now().UTC()), "previous_credential": tokenInfoOf(previous, m.now().UTC())}
			targetID = previous.ID
		case strings.HasPrefix(r.URL.Path, "/auth/principals/") && strings.HasSuffix(r.URL.Path, "/delete-preview") && r.Method == http.MethodPost:
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/auth/principals/"), "/delete-preview")
			agent, ok := s.Agents[id]
			if !ok || id == "" || strings.Contains(id, "/") {
				return nil, fail(http.StatusNotFound, "Agent not found")
			}
			view := m.principalViewProjected(*s, agent)
			result = principalDeletePreview{
				PrincipalID:          agent.ID,
				Name:                 agent.Name,
				GroupID:              agent.GroupID,
				Revision:             agent.Revision,
				PlanRevision:         s.AccessRevision,
				CredentialCount:      view.CredentialCount,
				ActiveMCPConnections: view.ActiveMCPConnections,
				ActiveRequests:       view.ActiveRequests,
				ActiveWork:           view.ActiveWork,
			}
			targetID = agent.ID
			accessRevisionChanged = false
		case strings.HasPrefix(r.URL.Path, "/auth/principals/") && r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/auth/principals/")
			agent, ok := s.Agents[id]
			if !ok || id == "" || strings.Contains(id, "/") {
				return nil, fail(http.StatusNotFound, "Agent not found")
			}
			var input struct {
				Revision     uint64 `json:"revision"`
				PlanRevision uint64 `json:"plan_revision"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil {
				return nil, fail(http.StatusBadRequest, "Agent deletion confirmation is required")
			}
			if input.Revision != agent.Revision || input.PlanRevision != s.AccessRevision {
				return nil, fail(http.StatusConflict, "Agent deletion inventory changed; preview again")
			}
			deletedTokens := principalTokenIDs(*s, agent.ID)
			for _, tokenID := range deletedTokens {
				delete(s.Tokens, tokenID)
			}
			canceledTokens = append(canceledTokens, deletedTokens...)
			for grantID, grant := range s.OAuthGrants {
				if grant.AgentID != agent.ID {
					continue
				}
				for signature, access := range s.OAuthAccess {
					if access.GrantID == grantID {
						canceledOAuth = append(canceledOAuth, oauthCancellation{principal: agent.ID, credential: access.ID})
						delete(s.OAuthAccess, signature)
					}
				}
				for signature, refresh := range s.OAuthRefresh {
					if refresh.GrantID == grantID {
						delete(s.OAuthRefresh, signature)
					}
				}
				delete(s.OAuthGrants, grantID)
			}
			delete(s.Agents, agent.ID)
			affectedPrincipals = append(affectedPrincipals, agent.ID)
			targetID = agent.ID
			result = map[string]any{"ok": true, "principal_id": agent.ID, "deleted_tokens": len(deletedTokens)}
		case strings.HasPrefix(r.URL.Path, "/auth/principals/") && r.Method == http.MethodPatch:
			id := strings.TrimPrefix(r.URL.Path, "/auth/principals/")
			agent, ok := s.Agents[id]
			if !ok || strings.Contains(id, "/") {
				return nil, fail(http.StatusNotFound, "Agent not found")
			}
			var input struct {
				Enabled  *bool  `json:"enabled"`
				Revision uint64 `json:"revision"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil || input.Enabled == nil {
				return nil, fail(http.StatusBadRequest, "enabled and revision are required")
			}
			if input.Revision != agent.Revision {
				return nil, fail(http.StatusConflict, "Agent changed; reload before saving")
			}
			agent.Enabled = *input.Enabled
			agent.Revision++
			s.Agents[id] = agent
			affectedPrincipals = append(affectedPrincipals, agent.ID)
			targetID = agent.ID
			result = map[string]any{"principal": agent}
		case r.URL.Path == "/auth/tokens/history" && r.Method == http.MethodDelete:
			principal := r.URL.Query().Get("principal_id")
			if principal == "" {
				principal = s.Admin.ID
			}
			if principal != s.Admin.ID {
				if _, ok := s.Agents[principal]; !ok {
					return nil, fail(http.StatusNotFound, "Agent not found")
				}
			}
			deleted := 0
			now := m.now().UTC()
			for id, token := range s.Tokens {
				if token.PrincipalID == principal && !tokenLive(token, now) {
					delete(s.Tokens, id)
					deleted++
				}
			}
			targetID = principal
			result = map[string]any{"ok": true, "principal_id": principal, "deleted_tokens": deleted}
		case strings.HasPrefix(r.URL.Path, "/auth/tokens/") && r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/auth/tokens/")
			token, ok := s.Tokens[id]
			if !ok || !tokenActive(token) {
				return nil, fail(http.StatusNotFound, "active token not found")
			}
			now := m.now().UTC()
			token.RevokedAt = &now
			s.Tokens[id] = token
			canceledTokens = append(canceledTokens, id)
			targetID = id
			result = map[string]bool{"ok": true}
		default:
			return nil, fail(http.StatusMethodNotAllowed, "method not allowed")
		}
		if _, preview := result.(principalDeletePreview); !preview {
			s.AccessRevision++
			accessRevisionChanged = true
		}
		return func() {
			m.mu.Lock()
			for _, tokenID := range canceledTokens {
				m.cancelCredentialLocked(tokenID)
			}
			m.cancelDeniedTokensLocked()
			m.cancelPrincipalsLocked(affectedPrincipals)
			m.mu.Unlock()
			m.cancelOAuthCredentialsMap(canceledOAuth)
		}, nil
	})
	if err != nil {
		if status, ok := err.(*statusError); ok {
			authError(w, status.status, status.err.Error())
		} else {
			authError(w, http.StatusInternalServerError, "could not save credentials; reload to check the result")
		}
		return
	}
	auditDetail(r.Context(), func(draft *auditDraft) {
		draft.targetID = targetID
		draft.beforeRevision = beforeAccessRevision
		if accessRevisionChanged {
			draft.afterRevision = beforeAccessRevision + 1
		}
	})
	authJSON(w, http.StatusOK, result)
}

func (m *Managed) principalViewProjected(s managedState, agent agentRecord) principalView {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.principalViewLocked(s, agent)
}

func (m *Managed) cancelOAuthCredentialsMap(records []oauthCancellation) {
	for _, record := range records {
		m.cancelCredentialLockedByID(record.principal, record.credential)
	}
}
