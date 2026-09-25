// Package schema composes the SpiceDB `agentsession` definition's
// grant-relation / check-permission block from the union of per-AgentClass
// (resourceType, permission) requirements declared via AgentSessionGrants
// CRs.
package schema

import "sort"

// GrantPair is one (resourceType, permission) tuple — the unit of schema
// composition. ResourceType must be a definition already present in the
// SpiceDB schema; Permission must be a permission declared on it.
type GrantPair struct {
	ResourceType string
	Permission   string
}

// RelationName returns the agentsession relation name for this pair:
// "grant_<permission>_<resourceType>". E.g., {"github_repo","admin"} →
// "grant_admin_github_repo".
func (p GrantPair) RelationName() string {
	return "grant_" + p.Permission + "_" + p.ResourceType
}

// PermissionName returns the agentsession permission name for this pair:
// "check_<permission>_<resourceType>".
func (p GrantPair) PermissionName() string {
	return "check_" + p.Permission + "_" + p.ResourceType
}

// DedupAndSort returns pairs uniquified and sorted by (ResourceType,
// Permission) for deterministic schema output.
func DedupAndSort(pairs []GrantPair) []GrantPair {
	seen := make(map[GrantPair]struct{}, len(pairs))
	out := make([]GrantPair, 0, len(pairs))
	for _, p := range pairs {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ResourceType != out[j].ResourceType {
			return out[i].ResourceType < out[j].ResourceType
		}
		return out[i].Permission < out[j].Permission
	})
	return out
}
