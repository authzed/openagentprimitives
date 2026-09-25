package slack

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// mentionForSubject reverses UserIdentity.status.channelIdentities to recover
// the Slack user id for a canonical subject in a given workspace (team_id), so
// an echo can render a real <@U…> mention. Returns ok=false when the user has
// no slack identity in that workspace (the caller renders an escaped display
// name instead). Read-only; never writes.
func mentionForSubject(ctx context.Context, k8s client.Client, canonical, teamID string) (string, bool) {
	var ui spiceboxv1alpha1.UserIdentity
	if err := k8s.Get(ctx, client.ObjectKey{Name: useridentity.NameForSubject(identity.Subject(canonical))}, &ui); err != nil {
		return "", false
	}
	for _, ci := range ui.Status.ChannelIdentities {
		if ci.Kind == "slack" && ci.Domain == teamID && ci.ExternalID != "" {
			return ci.ExternalID, true
		}
	}
	return "", false
}
