package projectors

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// providersProjector lists ClusterIdentityProvider CRs (cluster-scoped,
// singleton "default"). Primary condition is Valid (ConditionIdPValid).
type providersProjector struct{}

func (providersProjector) Resource() string { return "providers" }

func (providersProjector) List(ctx context.Context, c client.Client) ([]config.ResourceRow, error) {
	var list spiceboxv1alpha1.ClusterIdentityProviderList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	rows := make([]config.ResourceRow, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		status, reason := projectStatus(p.Status.Conditions, spiceboxv1alpha1.ConditionIdPValid, "Valid")

		badges := []config.Badge{{Key: "kind", Value: p.Spec.Kind}}
		if p.Spec.Issuer != "" {
			badges = append(badges, config.Badge{Key: "issuer", Value: p.Spec.Issuer})
		}

		rows = append(rows, config.ResourceRow{
			Name:         p.Name,
			Scope:        "cluster",
			Status:       status,
			StatusReason: reason,
			Badges:       badges,
			ManageCmd:    editCmd("clusteridentityprovider", p.Name, ""),
		})
	}
	return rows, nil
}

func init() { config.Register(&providersProjector{}) }
