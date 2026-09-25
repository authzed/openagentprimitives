package projectors

import (
	"context"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// identitiesProjector lists AgentIdentity CRs (namespaced).
type identitiesProjector struct{}

func (identitiesProjector) Resource() string { return "identities" }

func (identitiesProjector) List(ctx context.Context, c client.Client) ([]config.ResourceRow, error) {
	var list spiceboxv1alpha1.AgentIdentityList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	rows := make([]config.ResourceRow, 0, len(list.Items))
	for i := range list.Items {
		ai := &list.Items[i]
		status, reason := projectStatus(ai.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid, "Valid")

		var badges []config.Badge
		for _, k := range distinctCredentialTypes(ai.Spec.Credentials) {
			badges = append(badges, config.Badge{Key: "cred", Value: k})
		}

		rows = append(rows, config.ResourceRow{
			Name:         ai.Name,
			Namespace:    ai.Namespace,
			Scope:        "namespaced",
			Status:       status,
			StatusReason: reason,
			Badges:       badges,
			Counts: []config.Count{
				{Label: "credentials", Value: len(ai.Spec.Credentials)},
				{Label: "resolved", Value: len(ai.Status.ResolvedCredentials)},
			},
			ManageCmd: editCmd("agentidentity", ai.Name, ai.Namespace),
		})
	}
	return rows, nil
}

// distinctCredentialTypes returns the sorted set of credential type
// discriminators (static/oauth/federated) present on the identity.
func distinctCredentialTypes(creds []spiceboxv1alpha1.AgentCredential) []string {
	seen := map[string]struct{}{}
	for _, cr := range creds {
		if cr.Type != "" {
			seen[cr.Type] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func init() { config.Register(&identitiesProjector{}) }
