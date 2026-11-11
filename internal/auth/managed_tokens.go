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

var (
	errActiveCredential   = errors.New("principal already has an active MCP token; rotate it instead")
	errNoActiveCredential = errors.New("principal has no active MCP token")
)

type agentRecord struct {
	GroupID     string    `json:"group_id,omitempty"`
	Revision    uint64    `json:"revision,omitempty"`
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	OwnerUserID string    `json:"owner_user_id,omitempty"`
	Origin      string    `json:"origin,omitempty"`
}

func (a *agentRecord) UnmarshalJSON(raw []byte) error {
	type wire agentRecord
	var value wire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if value.Origin == "" {
		value.Origin = agentOriginManual
	}
	if value.Origin != agentOriginManual && value.Origin != agentOriginOAuth {
		return errors.New("invalid saved Agent origin")
	}
	*a = agentRecord(value)
	return nil
}

type tokenRecord struct {
	ID          string     `json:"id"`
	PrincipalID string     `json:"principal_id"`
	Label       string     `json:"label"`
	Digest      string     `json:"digest"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	ReplacedBy  string     `json:"replaced_by,omitempty"`
}

// tokenInfo is the only credential representation returned by listing APIs.
type tokenInfo struct {
	ID          string     `json:"id"`
	PrincipalID string     `json:"principal_id"`
	Label       string     `json:"label"`
	CreatedAt   time.Time  `json:"created_at"`
	Active      bool       `json:"active"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	ReplacedBy  string     `json:"replaced_by,omitempty"`
}

type principalView struct {
	agentRecord
	CredentialCount       int      `json:"credential_count"`
	ActiveCredentialCount int      `json:"active_credential_count"`
	ActiveMCPConnections  int      `json:"active_mcp_connections"`
	ActiveRequests        int      `json:"active_requests"`
	ActiveWork            int      `json:"active_work"`
	OwnerDisplayName      string   `json:"owner_display_name,omitempty"`
	OwnerUsername         string   `json:"owner_username,omitempty"`
	OwnerAvatarPath       string   `json:"owner_avatar_path,omitempty"`
	OAuthGrantCount       int      `json:"oauth_grant_count,omitempty"`
	ActiveOAuthGrantCount int      `json:"active_oauth_grant_count,omitempty"`
	OAuthClients          []string `json:"oauth_clients,omitempty"`
}

type principalDeletePreview struct {
	PrincipalID          string `json:"principal_id"`
	Name                 string `json:"name"`
	GroupID              string `json:"group_id"`
	Revision             uint64 `json:"revision"`
	PlanRevision         uint64 `json:"plan_revision"`
	CredentialCount      int    `json:"credential_count"`
	ActiveMCPConnections int    `json:"active_mcp_connections"`
	ActiveRequests       int    `json:"active_requests"`
	ActiveWork           int    `json:"active_work"`
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
		if a.Origin != agentOriginManual && a.Origin != agentOriginOAuth {
			return errors.New("invalid saved Agent origin")
		}
		if a.Origin == agentOriginOAuth && a.OwnerUserID == "" ||
			a.Origin == agentOriginManual && a.OwnerUserID != "" {
			return errors.New("invalid saved Agent owner")
		}
	}
	active := map[string]string{}
	for id, t := range s.Tokens {
		digest, err := hex.DecodeString(t.Digest)
		_, agent := s.Agents[t.PrincipalID]
		if id == "" || id != t.ID || !validLabel(t.Label) || err != nil || len(digest) != 32 || t.CreatedAt.IsZero() || t.PrincipalID != s.Admin.ID && !agent {
			return errors.New("invalid saved MCP token")
		}
		if t.RevokedAt != nil && t.RevokedAt.Before(t.CreatedAt) {
			return errors.New("invalid saved MCP token revocation")
		}
		if t.ExpiresAt != nil && !t.ExpiresAt.After(t.CreatedAt) {
			return errors.New("invalid saved MCP token expiry")
		}
		if t.LastUsedAt != nil && t.LastUsedAt.Before(t.CreatedAt) {
			return errors.New("invalid saved MCP token last use")
		}
		if t.ReplacedBy != "" {
			replacement, ok := s.Tokens[t.ReplacedBy]
			if !ok || replacement.PrincipalID != t.PrincipalID {
				return errors.New("invalid saved MCP token replacement")
			}
		}
		if t.RevokedAt == nil {
			if previous := active[t.PrincipalID]; previous != "" {
				return errors.New("principal has more than one active MCP token")
			}
			active[t.PrincipalID] = t.ID
		}
	}
	if err := validateGroupState(s); err != nil {
		return err
	}
	return validateOAuthServerState(s)
}

func tokenDigest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// SetMCPConnectionCounts publishes a fresh, immutable snapshot of live MCP
// sessions keyed by verified principal ID. The caller is the composition root,
// which can observe the MCP registry without making auth depend on internal/mcp.
func (m *Managed) SetMCPConnectionCounts(counts map[string]int) {
	if m == nil {
		return
	}
	snapshot := make(map[string]int, len(counts))
	for principalID, count := range counts {
		if principalID != "" && count > 0 {
			snapshot[principalID] = count
		}
	}
	m.mcpConnectionCounts.Store(&snapshot)
}

// SetMCPConnectionInvalidator wires authority changes to the MCP transport.
// The callback runs while auth state is locked, so the composition root must
// only schedule the transport cleanup and must not call back into Managed.
func (m *Managed) SetMCPConnectionInvalidator(fn func(principalID, credentialID string)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mcpConnectionInvalidator = fn
}

func (m *Managed) activeMCPConnections(principalID string) int {
	if m == nil || principalID == "" {
		return 0
	}
	snapshot := m.mcpConnectionCounts.Load()
	if snapshot == nil {
		return 0
	}
	return (*snapshot)[principalID]
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

// normalizeTokenUniqueness migrates pre-single-token state. The newest token is
// retained as the current credential; every older active record is revoked and
// linked to its replacement so it remains visible in history.
func normalizeTokenUniqueness(s *managedState, now time.Time) (bool, int) {
	byPrincipal := map[string][]tokenRecord{}
	for _, token := range s.Tokens {
		if tokenActive(token) {
			byPrincipal[token.PrincipalID] = append(byPrincipal[token.PrincipalID], token)
		}
	}
	changed := false
	revoked := 0
	for _, tokens := range byPrincipal {
		if len(tokens) < 2 {
			continue
		}
		sort.Slice(tokens, func(i, j int) bool {
			if tokens[i].CreatedAt.Equal(tokens[j].CreatedAt) {
				return tokens[i].ID < tokens[j].ID
			}
			return tokens[i].CreatedAt.After(tokens[j].CreatedAt)
		})
		keep := tokens[0]
		for _, token := range tokens[1:] {
			revokedAt := now
			token.RevokedAt = &revokedAt
			token.ReplacedBy = keep.ID
			s.Tokens[token.ID] = token
			changed = true
			revoked++
		}
	}
	return changed, revoked
}

func (m *Managed) principalViewLocked(s managedState, a agentRecord) principalView {
	view := principalView{
		agentRecord:          a,
		ActiveMCPConnections: m.activeMCPConnections(a.ID),
	}
	now := m.now().UTC()
	if owner, ok := s.LoginUsers[a.OwnerUserID]; ok {
		view.OwnerDisplayName = owner.DisplayName
		view.OwnerUsername = owner.Username
		if owner.AvatarFile != "" && s.OAuthServer.PublicURL != "" {
			view.OwnerAvatarPath = s.OAuthServer.PublicURL + "/auth/oauth/avatar/" + owner.AvatarFile
		}
	}
	clients := map[string]bool{}
	for _, grant := range s.OAuthGrants {
		if grant.AgentID != a.ID {
			continue
		}
		view.OAuthGrantCount++
		if grantActive(grant, now) {
			view.ActiveOAuthGrantCount++
		}
		if client := s.OAuthClients[grant.ClientID]; client.Name != "" {
			clients[client.Name] = true
		}
	}
	for name := range clients {
		view.OAuthClients = append(view.OAuthClients, name)
	}
	sort.Strings(view.OAuthClients)
	for id, token := range s.Tokens {
		if token.PrincipalID != a.ID {
			continue
		}
		view.CredentialCount++
		if tokenLive(token, now) {
			view.ActiveCredentialCount++
		}
		view.ActiveRequests += len(m.requests["token:"+id])
		view.ActiveWork += len(m.background[id])
	}
	for _, record := range s.OAuthAccess {
		grant := s.OAuthGrants[record.GrantID]
		if grant.AgentID != a.ID {
			continue
		}
		view.ActiveRequests += len(m.requests["token:"+record.ID])
		view.ActiveWork += len(m.background[record.ID])
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

func tokenActive(t tokenRecord) bool {
	return t.RevokedAt == nil
}

func tokenLive(t tokenRecord, now time.Time) bool {
	return tokenActive(t) && (t.ExpiresAt == nil || now.Before(*t.ExpiresAt))
}

func tokenStatus(t tokenRecord, now time.Time) string {
	if t.RevokedAt != nil {
		return "revoked"
	}
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return "expired"
	}
	return "active"
}

func tokenInfoOf(t tokenRecord, now time.Time) tokenInfo {
	status := tokenStatus(t, now)
	return tokenInfo{
		ID:          t.ID,
		PrincipalID: t.PrincipalID,
		Label:       t.Label,
		CreatedAt:   t.CreatedAt,
		Active:      status == "active",
		Status:      status,
		ExpiresAt:   t.ExpiresAt,
		LastUsedAt:  t.LastUsedAt,
		RevokedAt:   t.RevokedAt,
		ReplacedBy:  t.ReplacedBy,
	}
}

func activeTokenFor(s managedState, principalID string) (tokenRecord, bool) {
	for _, token := range s.Tokens {
		if token.PrincipalID == principalID && tokenActive(token) {
			return token, true
		}
	}
	return tokenRecord{}, false
}

func (m *Managed) principalLabel(s managedState, principal string) string {
	if principal == s.Admin.ID {
		return s.Admin.Username
	}
	return s.Agents[principal].Name
}

func (m *Managed) mintToken(s *managedState, principal, label string, expiresAt *time.Time) (tokenRecord, string, error) {
	if !validLabel(label) {
		return tokenRecord{}, "", errors.New("token label must contain 1–128 bytes without control characters")
	}
	if len(s.Tokens) >= maxTokens {
		return tokenRecord{}, "", errors.New("MCP token history limit reached; clear old token history")
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
	now := m.now().UTC()
	if expiresAt != nil {
		expiry := expiresAt.UTC()
		if !expiry.After(now) {
			return tokenRecord{}, "", errors.New("token expiry must be in the future")
		}
		expiresAt = &expiry
	}
	t := tokenRecord{ID: id, PrincipalID: principal, Label: label, Digest: tokenDigest(raw), CreatedAt: now, ExpiresAt: expiresAt}
	s.Tokens[id] = t
	return t, raw, nil
}

func (m *Managed) issueToken(s *managedState, principal, label string, expiresAt *time.Time) (tokenRecord, string, error) {
	if principal != s.Admin.ID {
		if _, ok := s.Agents[principal]; !ok {
			return tokenRecord{}, "", errors.New("Agent does not exist")
		}
	}
	current, hasCurrent := activeTokenFor(*s, principal)
	if hasCurrent && tokenLive(current, m.now().UTC()) {
		return tokenRecord{}, "", errActiveCredential
	}
	if label == "" {
		label = m.principalLabel(*s, principal)
	}
	next, raw, err := m.mintToken(s, principal, label, expiresAt)
	if err != nil {
		return tokenRecord{}, "", err
	}
	if hasCurrent {
		now := m.now().UTC()
		current.RevokedAt = &now
		current.ReplacedBy = next.ID
		s.Tokens[current.ID] = current
	}
	return next, raw, nil
}

func (m *Managed) rotateToken(s *managedState, tokenID, label string, expiresAt *time.Time) (tokenRecord, tokenRecord, string, error) {
	old, ok := s.Tokens[tokenID]
	now := m.now().UTC()
	if !ok || !tokenLive(old, now) {
		return tokenRecord{}, tokenRecord{}, "", errNoActiveCredential
	}
	if label == "" {
		label = old.Label
	}
	next, raw, err := m.mintToken(s, old.PrincipalID, label, expiresAt)
	if err != nil {
		return tokenRecord{}, tokenRecord{}, "", err
	}
	old.RevokedAt = &now
	old.ReplacedBy = next.ID
	s.Tokens[old.ID] = old
	return old, next, raw, nil
}

func (m *Managed) tokenActorLocked(t tokenRecord) (Actor, bool) {
	if !tokenLive(t, m.now().UTC()) {
		return Actor{}, false
	}
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
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 8192 {
		return nil, nil, 401
	}
	raw := parts[1]
	if !strings.HasPrefix(raw, "tmcp_") {
		return m.admitOAuthToken(r, raw)
	}
	pair := strings.Split(strings.TrimPrefix(raw, "tmcp_"), ".")
	if len(pair) != 2 {
		return nil, nil, 401
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Version == 0 || m.vault.AuthenticationLocked() {
		return nil, nil, 423
	}
	t, ok := m.state.Tokens[pair[0]]
	if !ok || !tokenLive(t, m.now().UTC()) || !equalSecret(t.Digest, tokenDigest(raw)) || m.vault.AuthenticationLocked() {
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
	usedAt := m.now().UTC()
	t.LastUsedAt = &usedAt
	m.state.Tokens[t.ID] = t
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
	for id, token := range m.state.Tokens {
		if tokenActive(token) {
			ids[id] = true
		}
	}
	now := m.now().UTC()
	for signature, record := range m.state.OAuthAccess {
		if record.RevokedAt != nil || !now.Before(record.ExpiresAt) {
			m.cancelCredentialLocked(record.ID)
			continue
		}
		grant := m.state.OAuthGrants[record.GrantID]
		if err := m.oauthAuthorityLocked(signature, record.ID, grant.AgentID, record.GrantID); err != nil {
			m.cancelCredentialLocked(record.ID)
		}
	}
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
	if r.Method != http.MethodGet {
		m.credentialMutation(w, r)
		return
	}
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
		now := m.now().UTC()
		for _, t := range s.Tokens {
			tokens = append(tokens, tokenInfoOf(t, now))
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
		a := agentRecord{ID: id, Name: input.Name, GroupID: input.GroupID, Enabled: true, Revision: 1, CreatedAt: m.now().UTC(), Origin: agentOriginManual}
		s.Agents[id] = a
		markAuditTarget(a.ID)
		result = map[string]any{"principal": a}
		if input.IssueToken {
			t, raw, err := m.issueToken(&s, id, input.Name, nil)
			if err != nil {
				authError(w, 400, err.Error())
				return
			}
			result = map[string]any{"principal": a, "token": raw, "credential": tokenInfoOf(t, m.now().UTC())}
			auditDetail(r.Context(), func(draft *auditDraft) {
				draft.action = "principal.create_with_credential"
			})
		}
	case r.URL.Path == "/auth/tokens" && r.Method == "POST":
		var input struct {
			PrincipalID string     `json:"principal_id"`
			Label       string     `json:"label"`
			ExpiresAt   *time.Time `json:"expires_at"`
		}
		if decodeJSON(http.MaxBytesReader(w, r.Body, 262144), &input) != nil {
			authError(w, 400, "invalid token request")
			return
		}
		principal := input.PrincipalID
		if principal == "" {
			principal = s.Admin.ID
		}
		t, raw, err := m.issueToken(&s, principal, input.Label, input.ExpiresAt)
		if err != nil {
			status := 400
			if errors.Is(err, errActiveCredential) {
				status = 409
			}
			authError(w, status, err.Error())
			return
		}
		result = map[string]any{"token": raw, "credential": tokenInfoOf(t, m.now().UTC())}
		markAuditTarget(t.ID)
	case strings.HasPrefix(r.URL.Path, "/auth/tokens/") && strings.HasSuffix(r.URL.Path, "/rotate") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/auth/tokens/"), "/rotate")
		if id == "" || strings.Contains(id, "/") {
			authError(w, 404, "active token not found")
			return
		}
		var input struct {
			Label     string     `json:"label"`
			ExpiresAt *time.Time `json:"expires_at"`
		}
		if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil {
			authError(w, 400, "invalid token rotation request")
			return
		}
		previous, next, raw, err := m.rotateToken(&s, id, input.Label, input.ExpiresAt)
		if err != nil {
			status := 400
			if errors.Is(err, errNoActiveCredential) {
				status = 404
			}
			authError(w, status, err.Error())
			return
		}
		deletedTokenIDs = append(deletedTokenIDs, previous.ID)
		result = map[string]any{
			"token":               raw,
			"credential":          tokenInfoOf(next, m.now().UTC()),
			"previous_credential": tokenInfoOf(previous, m.now().UTC()),
		}
		markAuditTarget(previous.ID)
	case strings.HasPrefix(r.URL.Path, "/auth/principals/") && strings.HasSuffix(r.URL.Path, "/delete-preview") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/auth/principals/"), "/delete-preview")
		a, ok := s.Agents[id]
		if !ok || id == "" || strings.Contains(id, "/") {
			authError(w, 404, "Agent not found")
			return
		}
		view := m.principalViewLocked(s, a)
		result = principalDeletePreview{
			PrincipalID:          a.ID,
			Name:                 a.Name,
			GroupID:              a.GroupID,
			Revision:             agentRevision(a),
			PlanRevision:         s.AccessRevision,
			CredentialCount:      view.CredentialCount,
			ActiveMCPConnections: view.ActiveMCPConnections,
			ActiveRequests:       view.ActiveRequests,
			ActiveWork:           view.ActiveWork,
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
		for grantID, grant := range s.OAuthGrants {
			if grant.AgentID != a.ID {
				continue
			}
			for signature, access := range s.OAuthAccess {
				if access.GrantID == grantID {
					deletedTokenIDs = append(deletedTokenIDs, access.ID)
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
	case r.URL.Path == "/auth/tokens/history" && r.Method == "DELETE":
		principal := r.URL.Query().Get("principal_id")
		if principal == "" {
			principal = s.Admin.ID
		}
		if principal != s.Admin.ID {
			if _, ok := s.Agents[principal]; !ok {
				authError(w, 404, "Agent not found")
				return
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
		markAuditTarget(principal)
		result = map[string]any{"ok": true, "principal_id": principal, "deleted_tokens": deleted}
	case strings.HasPrefix(r.URL.Path, "/auth/tokens/") && r.Method == "DELETE":
		id := strings.TrimPrefix(r.URL.Path, "/auth/tokens/")
		token, ok := s.Tokens[id]
		if !ok || !tokenActive(token) {
			authError(w, 404, "active token not found")
			return
		}
		now := m.now().UTC()
		token.RevokedAt = &now
		s.Tokens[id] = token
		deletedTokenIDs = append(deletedTokenIDs, id)
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
