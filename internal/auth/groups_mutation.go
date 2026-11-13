package auth

import (
	"errors"
	"net/http"
	"slices"
	"strings"
)

// groupMutation is the write path for permission groups. It validates and
// mutates a candidate snapshot, then lets commitState perform the encrypted
// write without holding the ordinary state mutex.
func (m *Managed) groupMutation(w http.ResponseWriter, r *http.Request) {
	var affected []string
	var result any
	mutated := false
	policyChanged := false
	targetID := auditTargetID(r.URL.Path)
	beforeAccessRevision := m.accessRevision()
	err := m.commitState(r.Context(), func(s *managedState) (func(), error) {
		mutated = true
		fail := func(status int, message string) error {
			return &statusError{status: status, err: errors.New(message)}
		}
		switch {
		case r.URL.Path == "/auth/groups" && r.Method == http.MethodPost:
			var input struct {
				Name        string       `json:"name"`
				Description string       `json:"description"`
				PresetID    string       `json:"preset_id"`
				Policy      *AgentPolicy `json:"policy"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 262144), &input) != nil || !validLabel(input.Name) || len(input.Description) > 2048 {
				return nil, fail(http.StatusBadRequest, "a valid group name and description are required")
			}
			if !groupNameAvailable(*s, input.Name, "") {
				return nil, fail(http.StatusConflict, "permission group name already exists")
			}
			if len(s.Groups) >= maxPermissionGroups {
				return nil, fail(http.StatusConflict, "permission group limit reached")
			}
			if input.PresetID != "" && input.Policy != nil {
				return nil, fail(http.StatusBadRequest, "preset_id and policy cannot be combined")
			}
			policy := EmptyAgentPolicy()
			presetVersion := 0
			if input.PresetID != "" {
				found := false
				for _, preset := range groupPresets() {
					if preset.ID != input.PresetID {
						continue
					}
					policy = clonePolicy(preset.Policy)
					presetVersion = preset.Version
					found = true
					if input.Description == "" {
						input.Description = preset.Description
					}
					break
				}
				if !found {
					return nil, fail(http.StatusBadRequest, "unknown group preset")
				}
			}
			if input.Policy != nil {
				policy = clonePolicy(*input.Policy)
			}
			if err := validateAgentPolicy(policy); err != nil {
				return nil, fail(http.StatusBadRequest, err.Error())
			}
			id, err := randomSecret()
			if err != nil {
				return nil, err
			}
			group := permissionGroup{ID: id, Name: input.Name, Description: input.Description, Enabled: true, Revision: 1, CreatedAt: m.now().UTC(), Policy: policy, PolicyVocabulary: permissionPolicyVocabularyVersion, PresetID: input.PresetID, PresetVersion: presetVersion}
			s.Groups[id] = group
			targetID = group.ID
			result = map[string]any{"group": m.groupViewProjected(*s, group)}
		case r.URL.Path == "/auth/default-group" && r.Method == http.MethodPut:
			var input struct {
				GroupID  string `json:"group_id"`
				Revision uint64 `json:"revision"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil {
				return nil, fail(http.StatusBadRequest, "invalid default group request")
			}
			if input.Revision != s.AccessRevision {
				return nil, fail(http.StatusConflict, "access state changed; reload before saving")
			}
			group, ok := s.Groups[input.GroupID]
			if !ok || !group.Enabled {
				return nil, fail(http.StatusBadRequest, "replacement group is missing or disabled")
			}
			for id, agent := range s.Agents {
				if agent.GroupID == "" {
					affected = append(affected, id)
					agent.Revision++
					s.Agents[id] = agent
				}
			}
			policyChanged = len(affected) > 0
			s.DefaultGroupID = group.ID
			targetID = group.ID
			result = map[string]any{"group_id": group.ID, "revision": s.AccessRevision + 1}
		case strings.HasPrefix(r.URL.Path, "/auth/principals/") && strings.HasSuffix(r.URL.Path, "/group") && r.Method == http.MethodPut:
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/auth/principals/"), "/")
			if len(parts) != 2 {
				return nil, fail(http.StatusMethodNotAllowed, "method not allowed")
			}
			agent, ok := s.Agents[parts[0]]
			if !ok {
				return nil, fail(http.StatusNotFound, "Agent not found")
			}
			var input struct {
				GroupID  string `json:"group_id"`
				Revision uint64 `json:"revision"`
			}
			if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil {
				return nil, fail(http.StatusBadRequest, "invalid group binding")
			}
			if input.Revision != agent.Revision {
				return nil, fail(http.StatusConflict, "Agent changed; reload before saving")
			}
			if input.GroupID != "" && input.GroupID != deniedGroupID {
				group, ok := s.Groups[input.GroupID]
				if !ok || !group.Enabled {
					return nil, fail(http.StatusBadRequest, "permission group is missing or disabled")
				}
			}
			agent.GroupID = input.GroupID
			agent.Revision++
			s.Agents[agent.ID] = agent
			affected = append(affected, agent.ID)
			policyChanged = true
			targetID = agent.ID
			result = map[string]any{"principal": agent}
		case strings.HasPrefix(r.URL.Path, "/auth/groups/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/auth/groups/"), "/")
			group, ok := s.Groups[parts[0]]
			if !ok {
				return nil, fail(http.StatusNotFound, "permission group not found")
			}
			members := groupMemberIDs(*s, group.ID)
			switch {
			case len(parts) == 2 && parts[1] == "delete-preview" && r.Method == http.MethodPost:
				view := m.groupViewProjected(*s, group)
				result = map[string]any{"group_id": group.ID, "revision": group.Revision, "plan_revision": s.AccessRevision, "member_count": view.MemberCount, "credential_count": view.CredentialCount, "active_requests": view.ActiveRequests, "active_work": view.ActiveWork, "members": members, "is_default": view.IsDefault}
				mutated = false
			case len(parts) == 2 && parts[1] == "clone" && r.Method == http.MethodPost:
				var input struct {
					Name string `json:"name"`
				}
				if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil || !validLabel(input.Name) {
					return nil, fail(http.StatusBadRequest, "a valid group name is required")
				}
				if !groupNameAvailable(*s, input.Name, "") {
					return nil, fail(http.StatusConflict, "permission group name already exists")
				}
				if len(s.Groups) >= maxPermissionGroups {
					return nil, fail(http.StatusConflict, "permission group limit reached")
				}
				id, err := randomSecret()
				if err != nil {
					return nil, err
				}
				group.ID = id
				group.Name = input.Name
				group.Revision = 1
				group.CreatedAt = m.now().UTC()
				group.Policy = clonePolicy(group.Policy)
				group.PolicyVocabulary = permissionPolicyVocabularyVersion
				group.PolicyWarnings = slices.Clone(group.PolicyWarnings)
				s.Groups[id] = group
				targetID = group.ID
				result = map[string]any{"group": m.groupViewProjected(*s, group)}
			case len(parts) == 1 && r.Method == http.MethodPatch:
				var input struct {
					Name        *string      `json:"name"`
					Description *string      `json:"description"`
					Enabled     *bool        `json:"enabled"`
					Policy      *AgentPolicy `json:"policy"`
					Revision    uint64       `json:"revision"`
				}
				if decodeJSON(http.MaxBytesReader(w, r.Body, 262144), &input) != nil || input.Name == nil && input.Description == nil && input.Enabled == nil && input.Policy == nil {
					return nil, fail(http.StatusBadRequest, "group changes are required")
				}
				if input.Revision != group.Revision {
					return nil, fail(http.StatusConflict, "group changed; reload before saving")
				}
				if input.Name != nil {
					if !validLabel(*input.Name) {
						return nil, fail(http.StatusBadRequest, "invalid group name")
					}
					if !groupNameAvailable(*s, *input.Name, group.ID) {
						return nil, fail(http.StatusConflict, "permission group name already exists")
					}
					group.Name = *input.Name
				}
				if input.Description != nil {
					if len(*input.Description) > 2048 {
						return nil, fail(http.StatusBadRequest, "description is too long")
					}
					group.Description = *input.Description
				}
				if input.Policy != nil {
					if err := validateAgentPolicy(*input.Policy); err != nil {
						return nil, fail(http.StatusBadRequest, err.Error())
					}
					group.Policy = clonePolicy(*input.Policy)
					group.PolicyVocabulary = permissionPolicyVocabularyVersion
					group.PolicyWarnings = nil
					policyChanged = true
				}
				if input.Enabled != nil {
					group.Enabled = *input.Enabled
					policyChanged = true
				}
				group.Revision++
				s.Groups[group.ID] = group
				for _, id := range members {
					agent := s.Agents[id]
					agent.Revision++
					s.Agents[id] = agent
				}
				affected = append(affected, members...)
				result = map[string]any{"group": m.groupViewProjected(*s, group)}
			case len(parts) == 1 && r.Method == http.MethodDelete:
				var input struct {
					Revision           uint64 `json:"revision"`
					PlanRevision       uint64 `json:"plan_revision"`
					Resolution         string `json:"resolution"`
					ReplacementGroupID string `json:"replacement_group_id"`
					DefaultGroupID     string `json:"default_group_id"`
				}
				if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil {
					return nil, fail(http.StatusBadRequest, "deletion confirmation is required")
				}
				if input.Revision != group.Revision || input.PlanRevision != s.AccessRevision {
					return nil, fail(http.StatusConflict, "deletion inventory changed; preview again")
				}
				if len(members) > 0 && input.Resolution != "deny" && input.Resolution != "move" {
					return nil, fail(http.StatusBadRequest, "choose move or deny for existing members")
				}
				if input.Resolution == "move" {
					replacement, ok := s.Groups[input.ReplacementGroupID]
					if !ok || !replacement.Enabled || replacement.ID == group.ID {
						return nil, fail(http.StatusBadRequest, "replacement group is missing or disabled")
					}
				}
				if s.DefaultGroupID == group.ID {
					replacement, ok := s.Groups[input.DefaultGroupID]
					if !ok || !replacement.Enabled || replacement.ID == group.ID {
						return nil, fail(http.StatusBadRequest, "deleting the default group requires a new default")
					}
					s.DefaultGroupID = replacement.ID
				}
				for _, id := range members {
					agent := s.Agents[id]
					if input.Resolution == "move" {
						agent.GroupID = input.ReplacementGroupID
					} else {
						agent.GroupID = deniedGroupID
					}
					agent.Revision++
					s.Agents[id] = agent
				}
				delete(s.Groups, group.ID)
				targetID = group.ID
				affected = append(affected, members...)
				policyChanged = true
				result = map[string]any{"ok": true, "affected_principals": len(members)}
			default:
				if len(parts) > 1 {
					return nil, fail(http.StatusNotFound, "group operation not found")
				}
				return nil, fail(http.StatusMethodNotAllowed, "method not allowed")
			}
		default:
			return nil, fail(http.StatusMethodNotAllowed, "method not allowed")
		}
		if mutated {
			s.AccessRevision++
		}
		var after func()
		if mutated {
			after = func() {
				m.mu.Lock()
				if policyChanged {
					m.cancelBackgroundForPrincipalsLocked(affected)
				}
				m.mu.Unlock()
			}
		}
		return after, nil
	})
	if err != nil {
		if status, ok := err.(*statusError); ok {
			authError(w, status.status, status.err.Error())
		} else {
			authError(w, http.StatusInternalServerError, "could not save access state; reload to check the result")
		}
		return
	}
	auditDetail(r.Context(), func(draft *auditDraft) {
		draft.targetID = targetID
		draft.beforeRevision = beforeAccessRevision
		if mutated {
			draft.afterRevision = beforeAccessRevision + 1
		}
		draft.affectedSubjects = len(affected)
	})
	authJSON(w, http.StatusOK, result)
}

func (m *Managed) accessRevision() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.AccessRevision
}

func (m *Managed) groupViewProjected(s managedState, group permissionGroup) groupView {
	view := groupView{permissionGroup: group, MemberCount: len(groupMemberIDs(s, group.ID)), IsDefault: s.DefaultGroupID == group.ID}
	now := m.now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	members := map[string]bool{}
	for _, id := range groupMemberIDs(s, group.ID) {
		members[id] = true
	}
	for id, token := range s.Tokens {
		if members[token.PrincipalID] {
			view.CredentialCount++
			if tokenLive(token, now) {
				view.ActiveCredentialCount++
			}
			view.ActiveRequests += len(m.requests["token:"+id])
			view.ActiveWork += len(m.background[id])
		}
	}
	for _, grant := range s.OAuthGrants {
		if !members[grant.AgentID] {
			continue
		}
		if grantActive(grant, now) {
			view.OAuthGrantCount++
		}
	}
	for _, record := range s.OAuthAccess {
		grant := s.OAuthGrants[record.GrantID]
		if !members[grant.AgentID] {
			continue
		}
		if record.RevokedAt == nil && now.Before(record.ExpiresAt) {
			view.ActiveOAuthCredentials++
		}
		view.ActiveRequests += len(m.requests["token:"+record.ID])
		view.ActiveWork += len(m.background[record.ID])
	}
	return view
}
