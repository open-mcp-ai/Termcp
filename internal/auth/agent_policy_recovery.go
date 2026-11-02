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
