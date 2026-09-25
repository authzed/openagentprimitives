package projectors

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// agentsProjector lists AgentClass CRs (namespaced).
type agentsProjector struct{}

func (agentsProjector) Resource() string { return "agents" }

func (agentsProjector) List(ctx context.Context, c client.Client) ([]config.ResourceRow, error) {
	var list spiceboxv1alpha1.AgentClassList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	rows := make([]config.ResourceRow, 0, len(list.Items))
	for i := range list.Items {
		ac := &list.Items[i]
		status, reason := projectStatus(ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, "Valid")

		var badges []config.Badge
		if ac.Spec.IdentityMode != "" {
			badges = append(badges, config.Badge{Key: "identityMode", Value: ac.Spec.IdentityMode})
		}
		// identityRef names the class's bound AgentIdentity (spec.agentIdentity)
		// so the Agents list can link the identity column to its detail page.
		if ac.Spec.AgentIdentity != "" {
			badges = append(badges, config.Badge{Key: "identityRef", Value: ac.Spec.AgentIdentity})
		}
		// oap flags a class installed from a `.oap` bundle, chipping its declared
		// version (falling back to a bare marker when the bundle manifest carried
		// none) so the list surfaces OAP provenance without a detail-page trip.
		if ac.Status.OapInstall != nil {
			v := ac.Status.OapInstall.Version
			if v == "" {
				v = "oap"
			}
			badges = append(badges, config.Badge{Key: "oap", Value: v})
		}

		// "tools" rolls up every tool-source ref the class declares: MCPServers,
		// SidecarToolboxes, and the toolkit-backed ToolBundles.
		toolCount := len(ac.Spec.MCPServers) + len(ac.Spec.SidecarToolboxes) + len(ac.Spec.ToolBundles)

		rows = append(rows, config.ResourceRow{
			Name:         ac.Name,
			Namespace:    ac.Namespace,
			Scope:        "namespaced",
			Status:       status,
			StatusReason: reason,
			Badges:       badges,
			Counts: []config.Count{
				{Label: "tools", Value: toolCount},
				{Label: "skills", Value: len(ac.Spec.Skills)},
			},
			ManageCmd: editCmd("agentclass", ac.Name, ac.Namespace),
		})
	}
	return rows, nil
}

func init() { config.Register(&agentsProjector{}) }
