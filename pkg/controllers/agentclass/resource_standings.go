package agentclass

import (
	"fmt"
	"sort"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// resolveResourceStandings answers, for EVERY resource type reachable from this
// class, how approval for it is governed.
//
// The sibling of resolveResourceDisplays and resolvePermissionTitles — same
// walk over the same fragments — but with one difference that matters: this one
// can FAIL. A display or a title that nobody declared degrades to a sensible
// fallback; a standing that nobody declared decides who may approve, and there
// is no fallback that is safe in both directions.
//
// STRICTEST fragment wins when several declare the same type, so composing an
// unrelated toolkit can never quietly weaken a type somebody marked required.
// The admin veto is applied by resolveStandingFor per type.
func resolveResourceStandings(
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
	veto map[string]struct{},
) ([]spiceboxv1alpha1.ResolvedResourceStanding, error) {
	seen := map[string]bool{}
	var types []string
	for _, res := range standingSources(mcpServers, toolkits, sidecarToolboxes) {
		if res.Name == "" || seen[res.Name] {
			continue
		}
		seen[res.Name] = true
		types = append(types, res.Name)
	}
	if len(types) == 0 {
		return nil, nil
	}
	sort.Strings(types)

	out := make([]spiceboxv1alpha1.ResolvedResourceStanding, 0, len(types))
	for _, t := range types {
		standing, approverPerm, err := resolveStandingFor(t, mcpServers, toolkits, sidecarToolboxes, veto)
		if err != nil {
			return nil, fmt.Errorf("spicedbSchema: %w", err)
		}
		out = append(out, spiceboxv1alpha1.ResolvedResourceStanding{
			ResourceType: t, Standing: standing, ApproverPermission: approverPerm,
		})
	}
	return out, nil
}

// standingVeto turns the admin's requireStandingFor list into the set shape the
// resolvers take. Defined here rather than duplicated at each call site so the
// slot path and the resource-type path can never disagree about what the veto
// contains.
func standingVeto(requireStandingFor []string) map[string]struct{} {
	if len(requireStandingFor) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(requireStandingFor))
	for _, t := range requireStandingFor {
		out[t] = struct{}{}
	}
	return out
}
