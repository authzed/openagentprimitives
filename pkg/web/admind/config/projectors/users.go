package projectors

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// usersProjector lists UserIdentity CRs (cluster-scoped). The displayed Name
// is the human identity (displayName, else subject) since metadata.name is an
// opaque hash of the subject. No ManageCmd is emitted: UserIdentity CRs are
// owned by the identity setup flow (the linking wizard), not hand-edited via
// `kubectl edit`, so surfacing an edit command would point operators at the
// wrong management path.
type usersProjector struct{}

func (usersProjector) Resource() string { return "users" }

func (usersProjector) List(ctx context.Context, c client.Client) ([]config.ResourceRow, error) {
	var list spiceboxv1alpha1.UserIdentityList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	rows := make([]config.ResourceRow, 0, len(list.Items))
	for i := range list.Items {
		ui := &list.Items[i]
		status, reason := projectStatus(ui.Status.Conditions, spiceboxv1alpha1.UserIdentityConditionValid, "Valid")

		display := ui.Spec.DisplayName
		if display == "" {
			display = ui.Spec.Subject
		}

		rows = append(rows, config.ResourceRow{
			Name:         display,
			Scope:        "cluster",
			Status:       status,
			StatusReason: reason,
			Badges:       []config.Badge{{Key: "subject", Value: ui.Spec.Subject}},
			Counts: []config.Count{
				{Label: "availableCredentials", Value: len(ui.Spec.Credentials)},
				{Label: "resolved", Value: len(ui.Status.ResolvedCredentials)},
			},
			// No ManageCmd: managed by the identity setup flow, not kubectl edit.
		})
	}
	return rows, nil
}

func init() { config.Register(&usersProjector{}) }
