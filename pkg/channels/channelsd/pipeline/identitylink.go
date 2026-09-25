package pipeline

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// upsertChannelIdentity inserts or updates ci in list, keyed by
// (Kind, Domain, ExternalID). Returns the new slice and whether anything
// changed (a byte-identical entry is a no-op → false, so the caller can skip
// the status write on the common repeat-message path). Multiple entries per
// user are expected — the same person across two workspaces.
func upsertChannelIdentity(list []spiceboxv1alpha1.ChannelIdentity, ci spiceboxv1alpha1.ChannelIdentity) ([]spiceboxv1alpha1.ChannelIdentity, bool) {
	for i := range list {
		if list[i].Kind == ci.Kind && list[i].Domain == ci.Domain && list[i].ExternalID == ci.ExternalID {
			if list[i].DisplayName == ci.DisplayName && list[i].Email == ci.Email {
				return list, false
			}
			list[i] = ci
			return list, true
		}
	}
	return append(list, ci), true
}

// recordChannelIdentity ensures a UserIdentity exists for the inbound sender's
// canonical subject and upserts the (kind, domain, externalID) channel identity
// onto its status.channelIdentities. Best-effort: no-ops for non-human inbounds,
// logs every failure with context, and never blocks message delivery.
func (p *Pipeline) recordChannelIdentity(ctx context.Context, ev channelkinds.InboundEvent) {
	if ev.ExternalIDs.ExternalID == "" {
		return // bento/service inbound: no human channel identity
	}
	logger := log.FromContext(ctx)
	// WIRE: subject is the full "user:<canonical>" SpiceDB subject ref.
	// UserIdentitySpec.Subject (a CRD field) stays string, so subject is
	// built as a string here and cast at the NameForSubject call below.
	subject := "user:" + canonicalID(ev.ExternalIDs).String()
	name := useridentity.NameForSubject(identity.Subject(subject))

	var ui spiceboxv1alpha1.UserIdentity
	getErr := p.K8s.Get(ctx, client.ObjectKey{Name: name}, &ui)
	switch {
	case apierrors.IsNotFound(getErr):
		// Ensure-create a minimal, credential-less UserIdentity (Valid=True, no
		// churn — see useridentity controller). Tolerate a create race.
		ui = spiceboxv1alpha1.UserIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: spiceboxv1alpha1.UserIdentitySpec{
				Subject:     subject,
				DisplayName: channelDisplay(ev.ExternalIDs),
			},
		}
		if cerr := p.K8s.Create(ctx, &ui); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			logger.Info("recordChannelIdentity: create UserIdentity failed",
				"subject", subject, "kind", ev.ExternalIDs.Kind, "externalID", ev.ExternalIDs.ExternalID, "err", cerr.Error())
			return
		}
		// Re-Get to obtain the server object (with resourceVersion) for the
		// status patch below. On a create race (AlreadyExists, swallowed above)
		// this picks up the winner's object.
		if gerr := p.K8s.Get(ctx, client.ObjectKey{Name: name}, &ui); gerr != nil {
			logger.Info("recordChannelIdentity: re-get after create failed",
				"subject", subject, "kind", ev.ExternalIDs.Kind, "externalID", ev.ExternalIDs.ExternalID, "err", gerr.Error())
			return
		}
	case getErr != nil:
		logger.Info("recordChannelIdentity: get UserIdentity failed",
			"subject", subject, "kind", ev.ExternalIDs.Kind, "externalID", ev.ExternalIDs.ExternalID, "err", getErr.Error())
		return
	}

	prior := ui.DeepCopy()
	next, changed := upsertChannelIdentity(ui.Status.ChannelIdentities, spiceboxv1alpha1.ChannelIdentity{
		Kind:        ev.ExternalIDs.Kind.String(),
		Domain:      ev.ExternalIDs.TeamScope.String(),
		ExternalID:  ev.ExternalIDs.ExternalID.String(),
		DisplayName: ev.ExternalIDs.DisplayName,
		Email:       ev.ExternalIDs.Email.String(),
	})
	if !changed {
		return // common path: identity already recorded, nothing to write
	}
	ui.Status.ChannelIdentities = next
	// Disjoint merge-patch: carries only the channelIdentities diff, never the
	// credential/condition fields the useridentity controller owns.
	if perr := p.K8s.Status().Patch(ctx, &ui, client.MergeFrom(prior)); perr != nil {
		logger.Info("recordChannelIdentity: status patch failed",
			"subject", subject, "kind", ev.ExternalIDs.Kind, "externalID", ev.ExternalIDs.ExternalID, "err", perr.Error())
	}
}

// channelDisplay is the human label for a new UserIdentity's spec.displayName:
// the channel display name if known, else the trusted email, else "".
func channelDisplay(id channelkinds.ExternalIdentity) string {
	if id.DisplayName != "" {
		return id.DisplayName
	}
	return id.Email.String()
}
