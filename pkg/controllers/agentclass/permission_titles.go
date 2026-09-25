package agentclass

import (
	"sort"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// resolvePermissionTitles collects every declared permission title reachable
// from this class, keyed by (resourceType, permission).
//
// FIRST declaration wins on a duplicate, and the result is sorted: composition
// is global, so two fragments can declare the same pair, and an order-dependent
// answer would rewrite status on every reconcile. Undeclared pairs are omitted
// rather than published empty — the card detokenizes the handle, and an empty
// string here would be indistinguishable from a title someone declared blank.
//
// A sibling of resolveSlotValueKeying (slot_valuekey.go), not a merge into it:
// that one walks TOOL permission checks (authz.Permission.Check) to derive
// value-keying chains; this one walks SCHEMA RESOURCES
// (SpiceDBSchemaFragment.Resources[].Permissions) to collect declared titles.
// Same three CR kinds, different part of them, different question.
func resolvePermissionTitles(
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
) []spiceboxv1alpha1.ResolvedPermissionTitle {
	type key struct{ resourceType, permission string }
	// Both strings the fragment declared about this permission: one for the
	// human deciding, one for the agent planning. Collected in ONE walk
	// because they share a key; a permission may declare either, both, or
	// neither.
	type prose struct{ title, planningNote string }
	seen := map[key]prose{}

	collect := func(f *spiceboxv1alpha1.SpiceDBSchemaFragment) {
		if f == nil {
			return
		}
		for _, res := range f.Resources {
			for _, p := range res.Permissions {
				if p.Title == "" && p.PlanningNote == "" {
					continue
				}
				k := key{res.Name, p.Name}
				if _, dup := seen[k]; !dup {
					seen[k] = prose{title: p.Title, planningNote: p.PlanningNote}
				}
			}
		}
	}
	for _, s := range mcpServers {
		collect(s.Spec.SpiceDBSchema)
	}
	for _, tk := range toolkits {
		collect(tk.Spec.SpiceDBSchema)
	}
	// SidecarToolbox fragments contribute here for the same reason they do to
	// standings and displays: a type whose only declaration is a sidecar's has
	// no other route to status, and an uncollected title is a card that
	// detokenizes the handle instead of showing the phrase its author wrote.
	for _, sc := range sidecarToolboxes {
		collect(sc.Spec.SpiceDBSchema)
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]spiceboxv1alpha1.ResolvedPermissionTitle, 0, len(seen))
	for k, p := range seen {
		out = append(out, spiceboxv1alpha1.ResolvedPermissionTitle{
			ResourceType: k.resourceType, Permission: k.permission,
			Title: p.title, PlanningNote: p.planningNote,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ResourceType != out[j].ResourceType {
			return out[i].ResourceType < out[j].ResourceType
		}
		return out[i].Permission < out[j].Permission
	})
	return out
}
