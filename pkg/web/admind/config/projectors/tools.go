package projectors

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// toolsProjector lists every tool-providing CRD under one slug: MCPServer +
// SidecarToolbox (namespaced) and SpiceboxToolkit + SpiceboxToolspec (cluster).
// Each row carries a "kind" badge so the UI can distinguish them.
type toolsProjector struct{}

func (toolsProjector) Resource() string { return "tools" }

func (toolsProjector) List(ctx context.Context, c client.Client) ([]config.ResourceRow, error) {
	var rows []config.ResourceRow

	// MCPServer — namespaced; health is the runtime socket via Reachable (not
	// the spec-Valid condition): a server can be Valid yet unreachable, and must
	// not read healthy. Absent Reachable → Unknown, the fail-closed default.
	// Mirrors the Channel projector's Connected precedent.
	var mcps spiceboxv1alpha1.MCPServerList
	if err := c.List(ctx, &mcps); err != nil {
		return nil, err
	}
	for i := range mcps.Items {
		m := &mcps.Items[i]
		status, reason := projectStatus(m.Status.Conditions, spiceboxv1alpha1.MCPServerConditionReachable, "Reachable")
		rows = append(rows, config.ResourceRow{
			Name:         m.Name,
			Namespace:    m.Namespace,
			Scope:        "namespaced",
			Status:       status,
			StatusReason: reason,
			Badges:       []config.Badge{{Key: "kind", Value: "mcpserver"}},
			Counts:       []config.Count{{Label: "observedTools", Value: len(m.Status.ObservedTools)}},
			ManageCmd:    editCmd("mcpserver", m.Name, m.Namespace),
		})
	}

	// SidecarToolbox — namespaced; primary condition Valid.
	var boxes spiceboxv1alpha1.SidecarToolboxList
	if err := c.List(ctx, &boxes); err != nil {
		return nil, err
	}
	for i := range boxes.Items {
		b := &boxes.Items[i]
		status, reason := projectSidecarToolboxStatus(b.Status.Conditions)
		rows = append(rows, config.ResourceRow{
			Name:         b.Name,
			Namespace:    b.Namespace,
			Scope:        "namespaced",
			Status:       status,
			StatusReason: reason,
			Badges:       []config.Badge{{Key: "kind", Value: "sidecartoolbox"}},
			Counts:       []config.Count{{Label: "observedTools", Value: len(b.Status.ObservedTools)}},
			ManageCmd:    editCmd("sidecartoolbox", b.Name, b.Namespace),
		})
	}

	// SpiceboxToolkit — CLUSTER-scoped; primary condition Valid.
	var kits spiceboxv1alpha1.SpiceboxToolkitList
	if err := c.List(ctx, &kits); err != nil {
		return nil, err
	}
	for i := range kits.Items {
		k := &kits.Items[i]
		status, reason := projectStatus(k.Status.Conditions, spiceboxv1alpha1.SpiceboxToolkitConditionValid, "Valid")
		rows = append(rows, config.ResourceRow{
			Name:         k.Name,
			Scope:        "cluster",
			Status:       status,
			StatusReason: reason,
			Badges:       []config.Badge{{Key: "kind", Value: "toolkit"}},
			Counts:       []config.Count{{Label: "subcommands", Value: len(k.Spec.Subcommands)}},
			ManageCmd:    editCmd("spiceboxtoolkit", k.Name, ""),
		})
	}

	// SpiceboxToolspec — CLUSTER-scoped; primary condition Valid.
	var specs spiceboxv1alpha1.SpiceboxToolspecList
	if err := c.List(ctx, &specs); err != nil {
		return nil, err
	}
	for i := range specs.Items {
		s := &specs.Items[i]
		status, reason := projectStatus(s.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid, "Valid")
		rows = append(rows, config.ResourceRow{
			Name:         s.Name,
			Scope:        "cluster",
			Status:       status,
			StatusReason: reason,
			Badges:       []config.Badge{{Key: "kind", Value: "toolspec"}},
			Counts:       []config.Count{{Label: "allowedSubcommands", Value: len(s.Spec.AllowSubcommands)}},
			ManageCmd:    editCmd("spiceboxtoolspec", s.Name, ""),
		})
	}

	return rows, nil
}

func init() { config.Register(&toolsProjector{}) }
