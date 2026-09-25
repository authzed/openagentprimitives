package agentclass

import (
	"sort"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// resolveResourceDisplays collects every declared resource display reachable
// from this class, keyed by resourceType. The sibling of
// resolvePermissionTitles for RESOURCE INSTANCE lines instead of permission
// lines — same walk, same determinism rules, different part of the fragment.
//
// FIRST declaration wins on a duplicate, and the result is sorted:
// composition is global, so two fragments can declare a display for the same
// type, and an order-dependent answer would rewrite status on every
// reconcile. A type with no declared Display is OMITTED rather than
// published with an empty one, so the card can tell "nobody declared a
// display" (fall back to the wire type name) from "somebody declared one
// with blank fields".
func resolveResourceDisplays(
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
) []spiceboxv1alpha1.ResolvedResourceDisplay {
	seen := map[string]spiceboxv1alpha1.SpiceDBResourceDisplay{}

	collect := func(f *spiceboxv1alpha1.SpiceDBSchemaFragment) {
		if f == nil {
			return
		}
		for _, res := range f.Resources {
			if res.Display == nil {
				continue
			}
			if _, dup := seen[res.Name]; !dup {
				seen[res.Name] = *res.Display
			}
		}
	}
	for _, s := range mcpServers {
		collect(s.Spec.SpiceDBSchema)
	}
	for _, tk := range toolkits {
		collect(tk.Spec.SpiceDBSchema)
	}
	// A SidecarToolbox fragment declares displays exactly as the other two do,
	// and for a type only IT declares — the workshop's draft — this walk is the
	// only way the phrase ever reaches status. Without it the approval card has
	// nothing to render but the wire handle, at the person who has to decide.
	for _, sc := range sidecarToolboxes {
		collect(sc.Spec.SpiceDBSchema)
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]spiceboxv1alpha1.ResolvedResourceDisplay, 0, len(seen))
	for resourceType, d := range seen {
		out = append(out, spiceboxv1alpha1.ResolvedResourceDisplay{
			ResourceType: resourceType, Name: d.Name, Icon: d.Icon, Label: d.Label,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceType < out[j].ResourceType })
	return out
}
