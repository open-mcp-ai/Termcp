package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

const maxAgents = 256
const maxTokens = 1024

type agentRecord struct {
	GroupID   string    `json:"group_id,omitempty"`
	Revision  uint64    `json:"revision,omitempty"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

func (a *agentRecord) UnmarshalJSON(raw []byte) error {
	type wire agentRecord
	var value wire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*a = agentRecord(value)
	return nil
}

type tokenRecord struct {
	ID          string    `json:"id"`
	PrincipalID string    `json:"principal_id"`
	Label       string    `json:"label"`
	Digest      string    `json:"digest"`
	CreatedAt   time.Time `json:"created_at"`
}

// tokenInfo is the only credential representation returned by listing APIs.
type tokenInfo struct {
	ID          string    `json:"id"`
	PrincipalID string    `json:"principal_id"`
	Label       string    `json:"label"`
	CreatedAt   time.Time `json:"created_at"`
}

type principalView struct {
	agentRecord
	CredentialCount int `json:"credential_count"`
	ActiveRequests  int `json:"active_requests"`
	ActiveWork      int `json:"active_work"`
}

type principalDeletePreview struct {
	PrincipalID     string `json:"principal_id"`
	Name            string `json:"name"`
	GroupID         string `json:"group_id"`
	Revision        uint64 `json:"revision"`
	PlanRevision    uint64 `json:"plan_revision"`
	CredentialCount int    `json:"credential_count"`
	ActiveRequests  int    `json:"active_requests"`
	ActiveWork      int    `json:"active_work"`
}

func validLabel(s string) bool {
	return s != "" && s == strings.TrimSpace(s) && len(s) <= 128 && !strings.ContainsFunc(s, unicode.IsControl)
}

func validateCredentials(s managedState) error {
	if len(s.Agents) > maxAgents || len(s.Tokens) > maxTokens {
		return errors.New("credential limit exceeded")
	}
	for id, a := range s.Agents {
		if a.Revision == 0 || id == "" || id != a.ID || id == s.Admin.ID || !validLabel(a.Name) || a.CreatedAt.IsZero() {
			return errors.New("invalid saved Agent")
		}
	}
	for id, t := range s.Tokens {
		digest, err := hex.DecodeString(t.Digest)
		_, agent := s.Agents[t.PrincipalID]
		if id == "" || id != t.ID || !validLabel(t.Label) || err != nil || len(digest) != 32 || t.CreatedAt.IsZero() || t.PrincipalID != s.Admin.ID && !agent {
			return errors.New("invalid saved MCP token")
		}
	}
	return validateGroupState(s)
}

func tokenDigest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func credentialState(s managedState) managedState {
	s.Groups = cloneGroups(s.Groups)
	s.Agents = maps.Clone(s.Agents)
	if s.Agents == nil {
		s.Agents = map[string]agentRecord{}
	}
	s.Tokens = maps.Clone(s.Tokens)
	if s.Tokens == nil {
		s.Tokens = map[string]tokenRecord{}
	}
	return s
}

func (m *Managed) principalViewLocked(s managedState, a agentRecord) principalView {
	view := principalView{agentRecord: a}
	for id, token := range s.Tokens {
		if token.PrincipalID != a.ID {
			continue
		}
		view.CredentialCount++
		view.ActiveRequests += len(m.requests["token:"+id])
		view.ActiveWork += len(m.background[id])
	}
	return view
}

func principalTokenIDs(s managedState, principalID string) []string {
	ids := []string{}
	for id, token := range s.Tokens {
		if token.PrincipalID == principalID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (m *Managed) issueToken(s *managedState, principal, label string) (tokenRecord, string, error) {
	if !validLabel(label) {
		return tokenRecord{}, "", errors.New("token label must contain 1–128 bytes without control characters")
	}
	if len(s.Tokens) >= maxTokens {
		return tokenRecord{}, "", errors.New("MCP token limit reached; revoke an unused token")
	}
	if principal != s.Admin.ID {
		if _, ok := s.Agents[principal]; !ok {
			return tokenRecord{}, "", errors.New("Agent does not exist")
		}
	}
	id, err := randomSecret()
	if err != nil {
		return tokenRecord{}, "", err
	}
	secret, err := randomSecret()
	if err != nil {
		return tokenRecord{}, "", err
	}
	raw := "tmcp_" + id + "." + secret
	t := tokenRecord{ID: id, PrincipalID: principal, Label: label, Digest: tokenDigest(raw), CreatedAt: m.now().UTC()}
	s.Tokens[id] = t
	return t, raw, nil
}

func (m *Managed) tokenActorLocked(t tokenRecord) (Actor, bool) {
	if t.PrincipalID == m.state.Admin.ID {
		return Actor{ID: m.state.Admin.ID, Username: m.state.Admin.Username, Role: RoleAdmin}, true
	}
	a, ok := m.state.Agents[t.PrincipalID]
	_, err := resolvedAgentPolicy(m.state, a)
	return Actor{ID: a.ID, Username: a.Name, Role: RoleAgent}, ok && a.Enabled && err == nil
}

// admitToken authenticates and registers cancellation under the same lock, so
// revocation cannot race between verification and attaching a new connection.
func (m *Managed) admitToken(r *http.Request) (context.Context, context.CancelFunc, int) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return nil, nil, 401
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 256 {
		return nil, nil, 401
	}
	raw := parts[1]
	pair := strings.Split(strings.TrimPrefix(raw, "tmcp_"), ".")
	if !strings.HasPrefix(raw, "tmcp_") || len(pair) != 2 {
		return nil, nil, 401
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Version == 0 || m.vault.AuthenticationLocked() {
		return nil, nil, 423
	}
	t, ok := m.state.Tokens[pair[0]]
	if !ok || !equalSecret(t.Digest, tokenDigest(raw)) || m.vault.AuthenticationLocked() {
		return nil, nil, 401
	}
	actor, ok := m.tokenActorLocked(t)
	if !ok {
		return nil, nil, 401
	}
	if requestSurface(r) != "mcp" {
		return nil, nil, 403
	}
	allowed := false
	for _, surface := range m.state.Permissions[actor.Role] {
		if surface == "mcp" {
			allowed = true
		}
	}
	if !allowed {
		return nil, nil, 403
	}
	ctx, cancel := context.WithCancel(r.Context())
	ref := new(int)
	id := "token:" + t.ID
	if m.requests[id] == nil {
		m.requests[id] = map[*int]context.CancelFunc{}
	}
	m.requests[id][ref] = cancel
	if actor.Role == RoleAgent {
		ctx = m.withAgentPolicy(ctx, actor.ID, t.ID)
		ctx = m.withBackgroundWork(ctx, actor.ID, t.ID)
	}
	ctx = context.WithValue(ctx, actorKey{}, actor)
	ctx = context.WithValue(ctx, credentialKey{}, id)
	return ctx, func() {
		cancel()
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.requests[id], ref)
		if len(m.requests[id]) == 0 {
			delete(m.requests, id)
		}
	}, 0
}

// cancelDeniedTokensLocked also handles global role policy changes.
func (m *Managed) cancelDeniedTokensLocked() {
	ids := map[string]bool{}
	for key := range m.requests {
		if strings.HasPrefix(key, "token:") {
			ids[strings.TrimPrefix(key, "token:")] = true
		}
	}
	for id := range m.background {
		ids[id] = true
	}
	for id := range ids {
		token, exists := m.state.Tokens[id]
		actor, enabled := m.tokenActorLocked(token)
		permitted := false
		for _, surface := range m.state.Permissions[actor.Role] {
			if surface == "mcp" {
				permitted = true
			}
		}
		if !exists || !enabled || !permitted {
			m.cancelCredentialLocked(id)
		}
	}
}

func (m *Managed) credentials(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := credentialState(m.state)
	var result any
	deletedTokenIDs := []string{}
	beforeAccessRevision := s.AccessRevision
	targetID := auditTargetID(r.URL.Path)
	markAuditTarget := func(id string) {
		if id != "" {
			targetID = id
		}
	}
	switch {
	case r.URL.Path == "/auth/principals" && r.Method == "GET":
		principals := make([]principalView, 0, len(s.Agents))
		for _, a := range s.Agents {
			principals = append(principals, m.principalViewLocked(s, a))
		}
		sort.Slice(principals, func(i, j int) bool { return principals[i].CreatedAt.Before(principals[j].CreatedAt) })
		authJSON(w, 200, map[string]any{"principals": principals, "default_group_id": s.DefaultGroupID, "access_revision": s.AccessRevision})
		return
	case r.URL.Path == "/auth/tokens" && r.Method == "GET":
		tokens := make([]tokenInfo, 0, len(s.Tokens))
		for _, t := range s.Tokens {
			tokens = append(tokens, tokenInfo{t.ID, t.PrincipalID, t.Label, t.CreatedAt})
		}
		sort.Slice(tokens, func(i, j int) bool { return tokens[i].CreatedAt.Before(tokens[j].CreatedAt) })
		authJSON(w, 200, map[string]any{"tokens": tokens})
		return
	case r.URL.Path == "/auth/principals" && r.Method == "POST":
		var input struct {
			Name       string `json:"name"`
			GroupID    string `json:"group_id"`
			IssueToken bool   `json:"issue_token"`
		}
		if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil || !validLabel(input.Name) {
			authError(w, 400, "a valid principal name and group binding are required")
			return
		}
		if len(s.Agents) >= maxAgents {
			authError(w, 409, "principal limit reached")
			return
		}
		if input.GroupID != "" {
			g, ok := s.Groups[input.GroupID]
			if !ok || !g.Enabled {
				authError(w, 400, "permission group is missing or disabled")
				return
			}
		}
		id, err := randomSecret()
		if err != nil {
			authError(w, 500, "could not create principal")
			return
		}
		a := agentRecord{ID: id, Name: input.Name, GroupID: input.GroupID, Enabled: true, Revision: 1, CreatedAt: m.now().UTC()}
		s.Agents[id] = a
		markAuditTarget(a.ID)
		result = map[string]any{"principal": a}
		if input.IssueToken {
			t, raw, err := m.issueToken(&s, id, input.Name)
			if err != nil {
				authError(w, 400, err.Error())
				return
			}
			result = map[string]any{"principal": a, "token": raw, "credential": tokenInfo{t.ID, t.PrincipalID, t.Label, t.CreatedAt}}
			auditDetail(r.Context(), func(draft *auditDraft) {
				draft.action = "principal.create_with_credential"
			})
		}
	case r.URL.Path == "/auth/tokens" && r.Method == "POST":
		var input struct {
			PrincipalID string `json:"principal_id"`
			Label       string `json:"label"`
		}
		if decodeJSON(http.MaxBytesReader(w, r.Body, 262144), &input) != nil {
			authError(w, 400, "invalid token request")
			return
		}
		principal := input.PrincipalID
		if principal == "" {
			principal = s.Admin.ID
		}
		t, raw, err := m.issueToken(&s, principal, input.Label)
		if err != nil {
			authError(w, 400, err.Error())
			return
		}
		result = map[string]any{"token": raw, "credential": tokenInfo{t.ID, t.PrincipalID, t.Label, t.CreatedAt}}
		markAuditTarget(t.ID)
	case strings.HasPrefix(r.URL.Path, "/auth/principals/") && strings.HasSuffix(r.URL.Path, "/delete-preview") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/auth/principals/"), "/delete-preview")
		a, ok := s.Agents[id]
		if !ok || id == "" || strings.Contains(id, "/") {
			authError(w, 404, "Agent not found")
			return
		}
		view := m.principalViewLocked(s, a)
		result = principalDeletePreview{
			PrincipalID:     a.ID,
			Name:            a.Name,
			GroupID:         a.GroupID,
			Revision:        agentRevision(a),
			PlanRevision:    s.AccessRevision,
			CredentialCount: view.CredentialCount,
			ActiveRequests:  view.ActiveRequests,
			ActiveWork:      view.ActiveWork,
		}
		markAuditTarget(a.ID)
		authJSON(w, 200, result)
		return
	case strings.HasPrefix(r.URL.Path, "/auth/principals/") && r.Method == "DELETE":
		id := strings.TrimPrefix(r.URL.Path, "/auth/principals/")
		a, ok := s.Agents[id]
		if !ok || id == "" || strings.Contains(id, "/") {
			authError(w, 404, "Agent not found")
			return
		}
		var input struct {
			Revision     uint64 `json:"revision"`
			PlanRevision uint64 `json:"plan_revision"`
		}
		if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil {
			authError(w, 400, "Agent deletion confirmation is required")
			return
		}
		if input.Revision != agentRevision(a) || input.PlanRevision != s.AccessRevision {
			authError(w, 409, "Agent deletion inventory changed; preview again")
			return
		}
		deletedTokenIDs = principalTokenIDs(s, a.ID)
		for _, tokenID := range deletedTokenIDs {
			delete(s.Tokens, tokenID)
		}
		delete(s.Agents, a.ID)
		markAuditTarget(a.ID)
		result = map[string]any{"ok": true, "principal_id": a.ID, "deleted_tokens": len(deletedTokenIDs)}
	case strings.HasPrefix(r.URL.Path, "/auth/principals/") && r.Method == "PATCH":
		id := strings.TrimPrefix(r.URL.Path, "/auth/principals/")
		a, ok := s.Agents[id]
		if !ok || strings.Contains(id, "/") {
			authError(w, 404, "Agent not found")
			return
		}
		var input struct {
			Enabled  *bool  `json:"enabled"`
			Revision uint64 `json:"revision"`
		}
		if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil || input.Enabled == nil {
			authError(w, 400, "enabled and revision are required")
			return
		}
		if input.Revision != agentRevision(a) {
			authError(w, 409, "Agent changed; reload before saving")
			return
		}
		a.Enabled = *input.Enabled
		a.Revision++
		s.Agents[id] = a
		markAuditTarget(a.ID)
		result = map[string]any{"principal": a}
	case strings.HasPrefix(r.URL.Path, "/auth/tokens/") && r.Method == "DELETE":
		id := strings.TrimPrefix(r.URL.Path, "/auth/tokens/")
		if _, ok := s.Tokens[id]; !ok {
			authError(w, 404, "token not found")
			return
		}
		delete(s.Tokens, id)
		markAuditTarget(id)
		result = map[string]bool{"ok": true}
	default:
		authError(w, 405, "method not allowed")
		return
	}
	s.AccessRevision++
	err := m.save(s, false)
	if err == nil || errors.Is(err, configstore.ErrDurability) {
		for _, tokenID := range deletedTokenIDs {
			m.cancelCredentialLocked(tokenID)
		}
		m.cancelDeniedTokensLocked()
		if strings.HasPrefix(r.URL.Path, "/auth/principals/") && r.Method == "PATCH" {
			principal := strings.TrimPrefix(r.URL.Path, "/auth/principals/")
			m.cancelPrincipalsLocked([]string{principal})
		}
	}
	if err != nil {
		authError(w, 500, "could not save credentials; reload to check the result")
		return
	}
	auditDetail(r.Context(), func(draft *auditDraft) {
		draft.targetID = targetID
		draft.beforeRevision = beforeAccessRevision
		draft.afterRevision = s.AccessRevision
	})
	authJSON(w, 200, result)
}
