package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"

	"github.com/open-mcp-ai/termcp/internal/authorization"
)

// AgentPolicy grants explicit semantic operations and saved host identities.
// Only schema 2 is supported; omitted operations are denied.
type AgentPolicy struct {
	SchemaVersion int                            `json:"schema_version,omitempty"`
	HostMode      string                         `json:"host_mode"`
	Hosts         []string                       `json:"hosts"`
	Actions       map[string]authorization.Grant `json:"actions"`
}

// Persistent state uses json.Unmarshal as well as the strict HTTP decoder.
// Keep policy decoding strict in both paths: ignoring an unknown condition on
// reopen would turn a narrower saved allow into a broader one.
func (p *AgentPolicy) UnmarshalJSON(raw []byte) error {
	type wire AgentPolicy
	var value wire
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	if value.SchemaVersion != 2 {
		return errors.New("unsupported permission schema: only schema_version 2 is accepted")
	}
	if value.SchemaVersion == 2 && value.Actions == nil {
		value.Actions = map[string]authorization.Grant{}
	}
	*p = AgentPolicy(value)
	return nil
}

// EmptyAgentPolicy is the explicit default for a newly created identity. Host
// scope does not itself authorize any action.
func EmptyAgentPolicy() AgentPolicy {
	return AgentPolicy{SchemaVersion: 2, HostMode: "all", Hosts: []string{}, Actions: map[string]authorization.Grant{}}
}

// AuthorizationPolicy projects the saved policy onto the pure decision model.
func (p AgentPolicy) AuthorizationPolicy() authorization.Policy {
	return authorization.Policy{HostMode: p.HostMode, Hosts: slices.Clone(p.Hosts), Actions: p.Actions}
}

func (p AgentPolicy) AllowsOperation(id string) bool {
	grant, ok := p.Actions[id]
	return ok && grant.Effect == authorization.EffectAllow
}

// MayRequestOperation reports whether an operation can enter the normal check
// path. EffectAsk belongs here: it queues unless the context is an approved
// replay, while output filtering must not mistake the persisted ask effect for
// a denial and hide the result after that replay succeeds.
func (p AgentPolicy) MayRequestOperation(id string) bool {
	grant, ok := p.Actions[id]
	return ok && (grant.Effect == authorization.EffectAllow || grant.Effect == authorization.EffectAsk)
}

func (p AgentPolicy) AllowsHost(id string) bool {
	return id != "" && (p.HostMode == "all" || p.HostMode == "selected" && slices.Contains(p.Hosts, id))
}
func clonePolicy(p AgentPolicy) AgentPolicy {
	p.Hosts = append([]string{}, p.Hosts...)
	p.Actions = mapsClone(p.Actions)
	return p
}

func cloneGrant(grant authorization.Grant) authorization.Grant {
	if grant.Constraints == nil {
		return grant
	}
	clone := *grant.Constraints
	clone.Keys = slices.Clone(clone.Keys)
	clone.ToolNames = slices.Clone(clone.ToolNames)
	clone.Methods = slices.Clone(clone.Methods)
	clone.Forms = slices.Clone(clone.Forms)
	if clone.RepeatMax != nil {
		n := *clone.RepeatMax
		clone.RepeatMax = &n
	}
	if clone.MaxBytes != nil {
		n := *clone.MaxBytes
		clone.MaxBytes = &n
	}
	grant.Constraints = &clone
	return grant
}

func mapsClone(in map[string]authorization.Grant) map[string]authorization.Grant {
	if in == nil {
		return nil
	}
	out := make(map[string]authorization.Grant, len(in))
	for id, grant := range in {
		out[id] = cloneGrant(grant)
	}
	return out
}
func agentRevision(a agentRecord) uint64 {
	return a.Revision
}

func validateAgentPolicy(p AgentPolicy) error {
	if p.SchemaVersion != 2 {
		return errors.New("schema_version must be 2 for an actions policy")
	}
	if p.HostMode != "all" && p.HostMode != "selected" && p.HostMode != "none" {
		return errors.New("host_mode must be all, selected or none")
	}
	if len(p.Hosts) > 1024 || p.HostMode != "selected" && len(p.Hosts) > 0 {
		return errors.New("invalid policy selection")
	}
	seen := map[string]bool{}
	for _, id := range p.Hosts {
		if id == "" || len(id) > 128 || seen[id] {
			return errors.New("invalid or duplicate host identity")
		}
		seen[id] = true
	}
	for id, grant := range p.Actions {
		if err := authorization.ValidateGrant(id, grant); err != nil {
			return err
		}
		if grant.Effect != authorization.EffectAllow && grant.Effect != authorization.EffectAsk {
			continue
		}
		if op, ok := authorization.Lookup(id); ok && op.Global && p.HostMode != "all" {
			return errors.New("operation " + id + " changes shared configuration and requires all hosts")
		}
	}
	return nil
}

type agentPolicyKey struct{}
type policyResolver func() (AgentPolicy, error)

// ApprovalContextForCredential reconstructs a verified Agent context for replay
// of a previously queued effect=ask operation. It uses current credential,
// principal and group state, so an approval cannot outlive a revocation.
func (m *Managed) ApprovalContextForCredential(ctx context.Context, principalID, credentialID string) (context.Context, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vault.AuthenticationLocked() {
		return nil, errors.New("configuration vault is locked")
	}
	t, ok := m.state.Tokens[credentialID]
	if !ok || t.PrincipalID != principalID {
		return nil, errors.New("approval credential is unavailable")
	}
	actor, ok := m.tokenActorLocked(t)
	if !ok || actor.Role != RoleAgent {
		return nil, errors.New("approval principal is disabled")
	}
	permitted := slices.Contains(m.state.Permissions[RoleAgent], "mcp")
	if !permitted {
		return nil, errors.New("Agent MCP access is disabled")
	}
	ctx = m.withAgentPolicy(ctx, actor.ID, credentialID)
	ctx = m.withBackgroundWork(ctx, actor.ID, credentialID)
	ctx = context.WithValue(ctx, actorKey{}, actor)
	ctx = context.WithValue(ctx, credentialKey{}, "token:"+credentialID)
	return ctx, nil
}

// PolicyFromContext consults live server state, never a client-provided policy.
func PolicyFromContext(ctx context.Context) (AgentPolicy, bool, error) {
	resolver, ok := ctx.Value(agentPolicyKey{}).(policyResolver)
	if !ok {
		return AgentPolicy{}, false, nil
	}
	if ctx.Err() != nil {
		return AgentPolicy{}, true, ctx.Err()
	}
	p, err := resolver()
	return p, true, err
}

func (m *Managed) withAgentPolicy(ctx context.Context, principal, credential string) context.Context {
	return context.WithValue(ctx, agentPolicyKey{}, policyResolver(func() (AgentPolicy, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		a, ok := m.state.Agents[principal]
		t, exists := m.state.Tokens[credential]
		permitted := slices.Contains(m.state.Permissions[RoleAgent], "mcp")
		if !ok || !a.Enabled || !exists || t.PrincipalID != principal || !permitted {
			return AgentPolicy{}, errors.New("Agent access has ended")
		}
		return resolvedAgentPolicy(m.state, a)
	}))
}

func (m *Managed) accessCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		authError(w, 405, "method not allowed")
		return
	}
	type host struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	hosts := []host{}
	if catalog, ok := m.vault.(interface {
		List() ([]string, error)
		Identity(string) (string, error)
	}); ok {
		names, err := catalog.List()
		if err != nil {
			authError(w, 500, "could not list saved hosts")
			return
		}
		for _, name := range names {
			if m.HideInternal && name == "internal" {
				continue
			}
			id, err := catalog.Identity(name)
			if err != nil {
				authError(w, 500, "could not resolve saved host identities")
				return
			}
			hosts = append(hosts, host{id, name})
		}
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
	authJSON(w, 200, map[string]any{
		"catalog_version":    4,
		"approval_supported": true,
		"operations":         authorization.Catalog(),
		"recipes":            authorization.Recipes(),
		"hosts":              hosts,
	})
}
