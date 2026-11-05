package auth

import (
	"slices"

	"github.com/open-mcp-ai/termcp/internal/authorization"
)

// repairAmbiguousAgentPolicies narrows grants whose old JSON representation may
// have dropped an empty allowlist. Keep the administrator able to log in and
// review them rather than failing the entire authentication vault at startup.
func repairAmbiguousAgentPolicies(state *managedState) bool {
	changed := false
	for id, group := range state.Groups {
		policy := clonePolicy(group.Policy)
		warnings := slices.Clone(group.PolicyWarnings)
		repaired := false
		for operation, grant := range policy.Actions {
			if grant.Effect != authorization.EffectAllow || grant.Constraints == nil || !grant.Constraints.LegacyAmbiguous(operation) {
				continue
			}
			policy.Actions[operation] = authorization.Grant{Effect: authorization.EffectDeny}
			if !slices.Contains(warnings, operation) {
				warnings = append(warnings, operation)
			}
			repaired = true
		}
		if !repaired {
			continue
		}
		slices.Sort(warnings)
		group.Policy = policy
		group.PolicyWarnings = warnings
		group.Revision++
		state.Groups[id] = group
		for principalID, agent := range state.Agents {
			if boundGroupID(*state, agent) == id {
				agent.Revision = agentRevision(agent) + 1
				state.Agents[principalID] = agent
			}
		}
		state.AccessRevision++
		changed = true
	}
	return changed
}

// legacyOperationSemanticsChanged lists IDs that survived the vocabulary
// consolidation but now cover more actions. An edited preset from the old
// vocabulary cannot be upgraded wholesale, so these grants must be dropped
// rather than silently widened.
var legacyOperationSemanticsChanged = map[string]bool{
	"cdp.open":     true,
	"ext.get":      true,
	"file.link":    true,
	"file.write":   true,
	"jrpc.open":    true,
	"mcp.open":     true,
	"profile.list": true,
}

// repairRetiredAgentOperations removes grants that belong to a retired or
// incompatibly broadened operation vocabulary. Unknown grants cannot authorize
// a current operation, but leaving them in place prevents the encrypted
// authentication state from loading at all. Drop them fail-closed, advance an
// edited built-in preset to the current version, and retain the names for review.
func repairRetiredAgentOperations(state *managedState) bool {
	changed := false
	for id, group := range state.Groups {
		policy := clonePolicy(group.Policy)
		warnings := slices.Clone(group.PolicyWarnings)
		repaired := false
		currentVersion := 0
		retiredPreset := group.PresetID != "" && group.PresetVersion > 0
		if retiredPreset {
			currentVersion = currentPresetVersion(group.PresetID)
			retiredPreset = currentVersion > group.PresetVersion
		}
		legacyVocabulary := group.PolicyVocabulary < permissionPolicyVocabularyVersion
		for operation := range policy.Actions {
			_, known := authorization.Lookup(operation)
			if known && (!legacyVocabulary || !legacyOperationSemanticsChanged[operation]) {
				continue
			}
			delete(policy.Actions, operation)
			warnings = addPolicyWarning(warnings, operation)
			repaired = true
		}
		if legacyVocabulary {
			group.PolicyVocabulary = permissionPolicyVocabularyVersion
			repaired = true
		}
		if retiredPreset {
			group.PresetVersion = currentVersion
			repaired = true
		}
		if !repaired {
			continue
		}
		slices.Sort(warnings)
		group.Policy = policy
		group.PolicyWarnings = warnings
		group.Revision++
		state.Groups[id] = group
		for principalID, agent := range state.Agents {
			if boundGroupID(*state, agent) == id {
				agent.Revision = agentRevision(agent) + 1
				state.Agents[principalID] = agent
			}
		}
		state.AccessRevision++
		changed = true
	}
	return changed
}
