package projectors

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// channelsProjector lists Channel CRs (namespaced). A channel's health is its
// runtime socket: the Connected condition (owned by channelsd), not the
// operator's spec-Valid condition — a Channel can be Valid yet disconnected.
type channelsProjector struct{}

func (channelsProjector) Resource() string { return "channels" }

func (channelsProjector) List(ctx context.Context, c client.Client) ([]config.ResourceRow, error) {
	var list spiceboxv1alpha1.ChannelList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	rows := make([]config.ResourceRow, 0, len(list.Items))
	for i := range list.Items {
		ch := &list.Items[i]
		status, reason := projectStatus(ch.Status.Conditions, spiceboxv1alpha1.ChannelConditionConnected, "Connected")

		badges := []config.Badge{{Key: "kind", Value: ch.Spec.Kind}}
		if ch.Spec.Role != "" {
			badges = append(badges, config.Badge{Key: "role", Value: ch.Spec.Role})
		}
		if ch.Spec.AgentClass != "" {
			badges = append(badges, config.Badge{Key: "agentClass", Value: ch.Spec.AgentClass})
		}

		rows = append(rows, config.ResourceRow{
			Name:         ch.Name,
			Namespace:    ch.Namespace,
			Scope:        "namespaced",
			Status:       status,
			StatusReason: reason,
			Badges:       badges,
			ManageCmd:    editCmd("channel", ch.Name, ch.Namespace),
		})
	}
	return rows, nil
}

func init() { config.Register(&channelsProjector{}) }
