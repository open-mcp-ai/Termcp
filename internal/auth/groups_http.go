package auth

import (
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

type groupView struct {
	permissionGroup
	MemberCount           int  `json:"member_count"`
	CredentialCount       int  `json:"credential_count"`
	ActiveCredentialCount int  `json:"active_credential_count"`
	ActiveRequests        int  `json:"active_requests"`
	ActiveWork            int  `json:"active_work"`
	IsDefault             bool `json:"is_default"`
}

func (m *Managed) groupViewLocked(s managedState, g permissionGroup) groupView {
	members := groupMemberIDs(s, g.ID)
	wanted := map[string]bool{}
	for _, id := range members {
		wanted[id] = true
	}
	out := groupView{permissionGroup: g, MemberCount: len(members), IsDefault: s.DefaultGroupID == g.ID}
	now := m.now().UTC()
	for id, token := range s.Tokens {
		if wanted[token.PrincipalID] {
			out.CredentialCount++
			if tokenLive(token, now) {
				out.ActiveCredentialCount++
			}
			out.ActiveRequests += len(m.requests["token:"+id])
			out.ActiveWork += len(m.background[id])
		}
	}
	return out
}
func isGroupPath(path string) bool {
	return path == "/auth/groups" || strings.HasPrefix(path, "/auth/groups/") || path == "/auth/group-presets" || path == "/auth/default-group" || (strings.HasPrefix(path, "/auth/principals/") && strings.HasSuffix(path, "/group"))
}

func (m *Managed) groupManagement(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := credentialState(m.state)
	affected := []string{}
	var result any
	beforeAccessRevision := s.AccessRevision
	targetID := auditTargetID(r.URL.Path)
	markAuditTarget := func(id string) {
		if id != "" {
			targetID = id
		}
	}
	fail := func(code int, message string) { authError(w, code, message) }
	if r.URL.Path == "/auth/group-presets" {
		if r.Method != "GET" {
			fail(405, "method not allowed")
			return
		}
		authJSON(w, 200, map[string]any{"presets": groupPresets()})
		return
	}
	if r.URL.Path == "/auth/groups" {
		switch r.Method {
		case "GET":
			groups := []groupView{}
			for _, g := range s.Groups {
				groups = append(groups, m.groupViewLocked(s, g))
			}
			sort.Slice(groups, func(i, j int) bool {
				if groups[i].IsDefault != groups[j].IsDefault {
					return groups[i].IsDefault
				}
				return groups[i].Name < groups[j].Name
			})
			authJSON(w, 200, map[string]any{"groups": groups, "default_group_id": s.DefaultGroupID, "access_revision": s.AccessRevision})
			return
		case "POST":
			var in struct {
				Name        string       `json:"name"`
				Description string       `json:"description"`
				PresetID    string       `json:"preset_id"`
				Policy      *AgentPolicy `json:"policy"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 262144), &in) != nil || !validLabel(in.Name) || len(in.Description) > 2048 {
				fail(400, "a valid group name and description are required")
				return
			}
			if !groupNameAvailable(s, in.Name, "") {
				fail(409, "permission group name already exists")
				return
			}
			if len(s.Groups) >= maxPermissionGroups {
				fail(409, "permission group limit reached")
				return
			}
			if in.PresetID != "" && in.Policy != nil {
				fail(400, "preset_id and policy cannot be combined")
				return
			}
			policy := EmptyAgentPolicy()
			presetVersion := 0
			if in.PresetID != "" {
				found := false
				for _, preset := range groupPresets() {
					if preset.ID == in.PresetID {
						policy = clonePolicy(preset.Policy)
						presetVersion = preset.Version
						found = true
						if in.Description == "" {
							in.Description = preset.Description
						}
						break
					}
				}
				if !found {
					fail(400, "unknown group preset")
					return
				}
			}
			if in.Policy != nil {
				policy = clonePolicy(*in.Policy)
			}
			if err := validateAgentPolicy(policy); err != nil {
				fail(400, err.Error())
				return
			}
			id, err := randomSecret()
			if err != nil {
				fail(500, "could not create group")
				return
			}
			group := permissionGroup{ID: id, Name: in.Name, Description: in.Description, Enabled: true, Revision: 1, CreatedAt: m.now().UTC(), Policy: policy, PolicyVocabulary: permissionPolicyVocabularyVersion, PresetID: in.PresetID, PresetVersion: presetVersion}
			s.Groups[id] = group
			markAuditTarget(group.ID)
			result = map[string]any{"group": m.groupViewLocked(s, group)}
		default:
			fail(405, "method not allowed")
			return
		}
	} else if r.URL.Path == "/auth/default-group" {
		if r.Method == "GET" {
			authJSON(w, 200, map[string]any{"group_id": s.DefaultGroupID, "revision": s.AccessRevision})
			return
		}
		if r.Method != "PUT" {
			fail(405, "method not allowed")
			return
		}
		var in struct {
			GroupID  string `json:"group_id"`
			Revision uint64 `json:"revision"`
		}
		if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &in) != nil {
			fail(400, "invalid default group request")
			return
		}
		if in.Revision != s.AccessRevision {
			fail(409, "access state changed; reload before saving")
			return
		}
		g, ok := s.Groups[in.GroupID]
		if !ok || !g.Enabled {
			fail(400, "replacement group is missing or disabled")
			return
		}
		for id, a := range s.Agents {
			if a.GroupID == "" {
				affected = append(affected, id)
				a.Revision = agentRevision(a) + 1
				s.Agents[id] = a
			}
		}
		s.DefaultGroupID = g.ID
		markAuditTarget(g.ID)
		result = map[string]any{"group_id": g.ID, "revision": s.AccessRevision + 1}
	} else if strings.HasPrefix(r.URL.Path, "/auth/principals/") {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/auth/principals/"), "/")
		if len(parts) != 2 || parts[1] != "group" || r.Method != "PUT" {
			fail(405, "method not allowed")
			return
		}
		a, ok := s.Agents[parts[0]]
		if !ok {
			fail(404, "Agent not found")
			return
		}
		var in struct {
			GroupID  string `json:"group_id"`
			Revision uint64 `json:"revision"`
		}
		if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &in) != nil {
			fail(400, "invalid group binding")
			return
		}
		if in.Revision != agentRevision(a) {
			fail(409, "Agent changed; reload before saving")
			return
		}
		if in.GroupID != "" && in.GroupID != deniedGroupID {
			group, ok := s.Groups[in.GroupID]
			if !ok || !group.Enabled {
				fail(400, "permission group is missing or disabled")
				return
			}
		}
		a.GroupID = in.GroupID
		a.Revision = agentRevision(a) + 1
		s.Agents[a.ID] = a
		markAuditTarget(a.ID)
		affected = append(affected, a.ID)
		result = map[string]any{"principal": a}
	} else {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/auth/groups/"), "/")
		group, ok := s.Groups[parts[0]]
		if !ok {
			fail(404, "permission group not found")
			return
		}
		members := groupMemberIDs(s, group.ID)
		if len(parts) > 1 && (len(parts) != 2 || parts[1] != "clone" && parts[1] != "delete-preview") {
			fail(404, "group operation not found")
			return
		}
		if len(parts) == 2 && parts[1] == "delete-preview" {
			if r.Method != "POST" {
				fail(405, "method not allowed")
				return
			}
			view := m.groupViewLocked(s, group)
			authJSON(w, 200, map[string]any{"group_id": group.ID, "revision": group.Revision, "plan_revision": s.AccessRevision, "member_count": view.MemberCount, "credential_count": view.CredentialCount, "active_requests": view.ActiveRequests, "active_work": view.ActiveWork, "members": members, "is_default": view.IsDefault})
			return
		}
		if len(parts) == 2 && parts[1] == "clone" {
			if r.Method != "POST" {
				fail(405, "method not allowed")
				return
			}
			var in struct {
				Name string `json:"name"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &in) != nil || !validLabel(in.Name) {
				fail(400, "a valid group name is required")
				return
			}
			if !groupNameAvailable(s, in.Name, "") {
				fail(409, "permission group name already exists")
				return
			}
			if len(s.Groups) >= maxPermissionGroups {
				fail(409, "permission group limit reached")
				return
			}
			id, err := randomSecret()
			if err != nil {
				fail(500, "could not clone group")
				return
			}
			group.ID = id
			group.Name = in.Name
			group.Revision = 1
			group.CreatedAt = m.now().UTC()
			group.Policy = clonePolicy(group.Policy)
			group.PolicyVocabulary = permissionPolicyVocabularyVersion
			group.PolicyWarnings = slices.Clone(group.PolicyWarnings)
			s.Groups[id] = group
			markAuditTarget(group.ID)
			result = map[string]any{"group": m.groupViewLocked(s, group)}
		} else if len(parts) == 1 && r.Method == "PATCH" {
			var in struct {
				Name        *string      `json:"name"`
				Description *string      `json:"description"`
				Enabled     *bool        `json:"enabled"`
				Policy      *AgentPolicy `json:"policy"`
				Revision    uint64       `json:"revision"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 262144), &in) != nil || in.Name == nil && in.Description == nil && in.Enabled == nil && in.Policy == nil {
				fail(400, "group changes are required")
				return
			}
			if in.Revision != group.Revision {
				fail(409, "group changed; reload before saving")
				return
			}
			if in.Name != nil {
				if !validLabel(*in.Name) {
					fail(400, "invalid group name")
					return
				}
				if !groupNameAvailable(s, *in.Name, group.ID) {
					fail(409, "permission group name already exists")
					return
				}
				group.Name = *in.Name
			}
			if in.Description != nil {
				if len(*in.Description) > 2048 {
					fail(400, "description is too long")
					return
				}
				group.Description = *in.Description
			}
			if in.Policy != nil {
				if err := validateAgentPolicy(*in.Policy); err != nil {
					fail(400, err.Error())
					return
				}
				group.Policy = clonePolicy(*in.Policy)
				group.PolicyVocabulary = permissionPolicyVocabularyVersion
				group.PolicyWarnings = nil
			}
			if in.Enabled != nil {
				group.Enabled = *in.Enabled
			}
			group.Revision++
			s.Groups[group.ID] = group
			for _, id := range members {
				a := s.Agents[id]
				a.Revision = agentRevision(a) + 1
				s.Agents[id] = a
			}
			affected = append(affected, members...)
			result = map[string]any{"group": m.groupViewLocked(s, group)}
		} else if len(parts) == 1 && r.Method == "DELETE" {
			var in struct {
				Revision           uint64 `json:"revision"`
				PlanRevision       uint64 `json:"plan_revision"`
				Resolution         string `json:"resolution"`
				ReplacementGroupID string `json:"replacement_group_id"`
				DefaultGroupID     string `json:"default_group_id"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &in) != nil {
				fail(400, "deletion confirmation is required")
				return
			}
			if in.Revision != group.Revision || in.PlanRevision != s.AccessRevision {
				fail(409, "deletion inventory changed; preview again")
				return
			}
			if len(members) > 0 && in.Resolution != "deny" && in.Resolution != "move" {
				fail(400, "choose move or deny for existing members")
				return
			}
			if in.Resolution == "move" {
				g, ok := s.Groups[in.ReplacementGroupID]
				if !ok || !g.Enabled || g.ID == group.ID {
					fail(400, "replacement group is missing or disabled")
					return
				}
			}
			if s.DefaultGroupID == group.ID {
				g, ok := s.Groups[in.DefaultGroupID]
				if !ok || !g.Enabled || g.ID == group.ID {
					fail(400, "deleting the default group requires a new default")
					return
				}
				s.DefaultGroupID = g.ID
			}
			for _, id := range members {
				a := s.Agents[id]
				if in.Resolution == "move" {
					a.GroupID = in.ReplacementGroupID
				} else {
					a.GroupID = deniedGroupID
				}
				a.Revision = agentRevision(a) + 1
				s.Agents[id] = a
			}
			delete(s.Groups, group.ID)
			markAuditTarget(group.ID)
			affected = append(affected, members...)
			result = map[string]any{"ok": true, "affected_principals": len(members)}
		} else {
			fail(405, "method not allowed")
			return
		}
	}
	s.AccessRevision++
	err := m.save(s, false)
	if err == nil || errors.Is(err, configstore.ErrDurability) {
		m.cancelDeniedTokensLocked()
		m.cancelPrincipalsLocked(affected)
	}
	if err != nil {
		fail(500, "could not save access state; reload to check the result")
		return
	}
	auditDetail(r.Context(), func(draft *auditDraft) {
		draft.targetID = targetID
		draft.beforeRevision = beforeAccessRevision
		draft.afterRevision = s.AccessRevision
		draft.affectedSubjects = len(affected)
	})
	authJSON(w, 200, result)
}
