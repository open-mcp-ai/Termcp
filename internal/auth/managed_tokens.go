package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	Policy    *AgentPolicy `json:"policy,omitempty"`
	Revision  uint64       `json:"revision,omitempty"`
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Enabled   bool         `json:"enabled"`
	CreatedAt time.Time    `json:"created_at"`
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

func validLabel(s string) bool {
	return s != "" && s == strings.TrimSpace(s) && len(s) <= 128 && !strings.ContainsFunc(s, unicode.IsControl)
}

func validateCredentials(s managedState) error {
	if len(s.Agents) > maxAgents || len(s.Tokens) > maxTokens {
		return errors.New("credential limit exceeded")
	}
	for id, a := range s.Agents {
		if a.Policy != nil {
			if err := validateAgentPolicy(*a.Policy); err != nil {
				return err
			}
		}
		if id == "" || id != a.ID || id == s.Admin.ID || !validLabel(a.Name) || a.CreatedAt.IsZero() {
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
	return nil
}

func tokenDigest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func credentialState(s managedState) managedState {
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
	return Actor{ID: a.ID, Username: a.Name, Role: RoleAgent}, ok && a.Enabled
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
	for id, requests := range m.requests {
		if !strings.HasPrefix(id, "token:") {
			continue
		}
		t, ok := m.state.Tokens[strings.TrimPrefix(id, "token:")]
		actor, enabled := m.tokenActorLocked(t)
		allowed := false
		for _, surface := range m.state.Permissions[actor.Role] {
			if surface == "mcp" {
				allowed = true
			}
		}
		if !ok || !enabled || !allowed {
			for _, cancel := range requests {
				cancel()
			}
		}
	}
}

func (m *Managed) credentials(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := credentialState(m.state)
	var result any
	switch {
	case r.URL.Path == "/auth/agents" && r.Method == "GET":
		agents := make([]agentRecord, 0, len(s.Agents))
		for _, a := range s.Agents {
			p := effectivePolicy(a)
			a.Policy = &p
			a.Revision = agentRevision(a)
			agents = append(agents, a)
		}
		sort.Slice(agents, func(i, j int) bool { return agents[i].CreatedAt.Before(agents[j].CreatedAt) })
		authJSON(w, 200, map[string]any{"agents": agents})
		return
	case r.URL.Path == "/auth/tokens" && r.Method == "GET":
		tokens := make([]tokenInfo, 0, len(s.Tokens))
		for _, t := range s.Tokens {
			tokens = append(tokens, tokenInfo{t.ID, t.PrincipalID, t.Label, t.CreatedAt})
		}
		sort.Slice(tokens, func(i, j int) bool { return tokens[i].CreatedAt.Before(tokens[j].CreatedAt) })
		authJSON(w, 200, map[string]any{"tokens": tokens})
		return
	case r.URL.Path == "/auth/agents" && r.Method == "POST":
		var input struct {
			Name       string       `json:"name"`
			Policy     *AgentPolicy `json:"policy"`
			IssueToken *bool        `json:"issue_token"`
		}
		if decodeJSON(http.MaxBytesReader(w, r.Body, 262144), &input) != nil || !validLabel(input.Name) {
			authError(w, 400, "a valid Agent name is required")
			return
		}
		if len(s.Agents) >= maxAgents {
			authError(w, 409, "Agent limit reached")
			return
		}
		id, err := randomSecret()
		if err != nil {
			authError(w, 500, "could not create Agent")
			return
		}
		if input.Policy != nil {
			if err := validateAgentPolicy(*input.Policy); err != nil {
				authError(w, 400, err.Error())
				return
			}
		}
		a := agentRecord{ID: id, Name: input.Name, Enabled: true, CreatedAt: m.now().UTC(), Policy: input.Policy, Revision: 1}
		s.Agents[id] = a
		if input.IssueToken != nil && !*input.IssueToken {
			result = map[string]any{"agent": a}
			break
		}
		t, raw, err := m.issueToken(&s, id, input.Name)
		if err != nil {
			authError(w, 400, err.Error())
			return
		}
		result = map[string]any{"agent": a, "token": raw, "credential": tokenInfo{t.ID, t.PrincipalID, t.Label, t.CreatedAt}}
	case r.URL.Path == "/auth/tokens" && r.Method == "POST":
		var input struct {
			AgentID string `json:"agent_id"`
			Label   string `json:"label"`
		}
		if decodeJSON(http.MaxBytesReader(w, r.Body, 262144), &input) != nil {
			authError(w, 400, "invalid token request")
			return
		}
		principal := input.AgentID
		if principal == "" {
			principal = s.Admin.ID
		}
		t, raw, err := m.issueToken(&s, principal, input.Label)
		if err != nil {
			authError(w, 400, err.Error())
			return
		}
		result = map[string]any{"token": raw, "credential": tokenInfo{t.ID, t.PrincipalID, t.Label, t.CreatedAt}}
	case strings.HasPrefix(r.URL.Path, "/auth/agents/") && r.Method == "PATCH":
		id := strings.TrimPrefix(r.URL.Path, "/auth/agents/")
		a, ok := s.Agents[id]
		if !ok {
			authError(w, 404, "Agent not found")
			return
		}
		var input struct {
			Enabled  *bool        `json:"enabled"`
			Policy   *AgentPolicy `json:"policy"`
			Revision *uint64      `json:"revision"`
		}
		if decodeJSON(http.MaxBytesReader(w, r.Body, 262144), &input) != nil || input.Enabled == nil && input.Policy == nil {
			authError(w, 400, "enabled or policy is required")
			return
		}
		if input.Revision != nil && *input.Revision != agentRevision(a) {
			authError(w, 409, "Agent changed; reload before saving")
			return
		}
		if input.Policy != nil {
			if input.Revision == nil {
				authError(w, 400, "policy updates require revision")
				return
			}
			if err := validateAgentPolicy(*input.Policy); err != nil {
				authError(w, 400, err.Error())
				return
			}
			a.Policy = input.Policy
		}
		if input.Enabled != nil {
			a.Enabled = *input.Enabled
		}
		a.Revision = agentRevision(a) + 1
		s.Agents[id] = a
		result = map[string]any{"agent": a}
	case strings.HasPrefix(r.URL.Path, "/auth/tokens/") && r.Method == "DELETE":
		id := strings.TrimPrefix(r.URL.Path, "/auth/tokens/")
		if _, ok := s.Tokens[id]; !ok {
			authError(w, 404, "token not found")
			return
		}
		delete(s.Tokens, id)
		result = map[string]bool{"ok": true}
	default:
		authError(w, 405, "method not allowed")
		return
	}
	err := m.save(s, false)
	if err == nil || errors.Is(err, configstore.ErrDurability) {
		m.cancelDeniedTokensLocked()
		if strings.HasPrefix(r.URL.Path, "/auth/agents/") && r.Method == "PATCH" {
			principal := strings.TrimPrefix(r.URL.Path, "/auth/agents/")
			for id, t := range m.state.Tokens {
				if t.PrincipalID == principal {
					for _, cancel := range m.requests["token:"+id] {
						cancel()
					}
				}
			}
		}
	}
	if err != nil {
		authError(w, 500, "could not save credentials; reload to check the result")
		return
	}
	authJSON(w, 200, result)
}
