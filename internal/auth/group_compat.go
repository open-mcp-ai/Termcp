package auth

import (
	"errors"
	"slices"
)

// setCompatibilityPolicy turns an old per-agent policy write into a one-member
// group. It never silently changes a group shared with other principals.
func setCompatibilityPolicy(s *managedState, a *agentRecord, policy AgentPolicy) error {
	group, ok := s.Groups[boundGroupID(*s, *a)]
	if !ok || group.LegacyPrincipalID != a.ID || len(groupMemberIDs(*s, group.ID)) != 1 {
		if len(s.Groups) >= maxPermissionGroups {
			return errors.New("permission group limit reached")
		}
		id, err := randomSecret()
		if err != nil {
			return err
		}
		group = permissionGroup{ID: id, Name: uniqueCompatibilityGroupName(*s, a.Name, a.ID), Enabled: true, Revision: 0, CreatedAt: a.CreatedAt, LegacyPrincipalID: a.ID}
	}
	group.Policy = clonePolicy(policy)
	group.PolicyWarnings = nil
	group.Revision++
	s.Groups[group.ID] = group
	a.GroupID = group.ID
	a.Policy = nil
	a.PolicyWarnings = nil
	return nil
}

func cloneWarningIDs(ids []string) []string { return slices.Clone(ids) }
