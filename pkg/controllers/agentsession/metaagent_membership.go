// pkg/controllers/agentsession/metaagent_membership.go
//
// ensureMetaagentChannelMembership reconciles the Channel.Status condition
// reflecting whether the metaagent bot is in the channel. Called from the
// AgentSession Reconcile loop when the bound AgentClass has
// spec.authz.scope.enabled=true. The per-transport answer is the channel
// kind's (channelkinds.MetaagentMembershipReporter), not this package's.
package agentsession

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// ensureMetaagentChannelMembership reconciles the Channel.Status condition
// reflecting whether the metaagent bot is in the channel. The answer belongs
// to the channel kind — whether a transport even HAS a bot roster, and what
// counts as the bot being reachable, are properties of the transport — so this
// resolves the kind through the registry and asks it, exactly as
// outputChannelGroupRef asks OwnerGroupProvider.
//
// No-ops when:
//   - ac is nil, or AgentClass scope.enabled=false.
//   - sess.Spec.InputChannel is nil (kubectl-driven session; no channel).
//   - The Channel CR is not found (not yet created; will reconcile again).
//   - The kind does not report membership (see MetaagentMembershipFor).
func (r *Reconciler) ensureMetaagentChannelMembership(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ac *spiceboxv1alpha1.AgentClass,
) error {
	if ac == nil || !ac.Spec.GetScope().Enabled {
		return nil
	}
	if sess.Spec.InputChannel == nil {
		return nil // kubectl-driven session: no channel to invite into.
	}

	var ch spiceboxv1alpha1.Channel
	chKey := client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.InputChannel.Name}
	if err := r.Client.Get(ctx, chKey, &ch); err != nil {
		if apierrors.IsNotFound(err) {
			// Channel not yet created; this reconcile loop will re-run when
			// the Channel CR appears (Watch on Channel). No-op here.
			return nil
		}
		return fmt.Errorf("ensureMetaagentChannelMembership: get channel: %w", err)
	}

	// spec.metaagent.enabled=false explicitly disables the invite.
	if ch.Spec.Metaagent != nil && ch.Spec.Metaagent.Enabled != nil && !*ch.Spec.Metaagent.Enabled {
		return r.patchChannelCondition(ctx, &ch, metav1.Condition{
			Type:    spiceboxv1alpha1.ChannelConditionMetaagentChannelMembership,
			Status:  metav1.ConditionFalse,
			Reason:  "DisabledByChannelOverride",
			Message: "spec.metaagent.enabled=false overrides AgentClass scope setting",
		})
	}

	// Ask the kind. An unregistered kind name yields a nil Kind, which
	// MetaagentMembershipFor reports as "not applicable" — the same answer as
	// a kind with no bot roster (fake, local, builtin, bento). Either way the
	// condition stays ABSENT, so a reader can tell "this transport has no such
	// concept" apart from "the bot is missing".
	k, _ := chregistry.Get(ch.Spec.Kind)
	m, ok := channelkinds.MetaagentMembershipFor(k, &ch)
	if !ok {
		return nil
	}
	return r.patchChannelCondition(ctx, &ch, metav1.Condition{
		Type:    spiceboxv1alpha1.ChannelConditionMetaagentChannelMembership,
		Status:  m.Status,
		Reason:  m.Reason,
		Message: m.Message,
	})
}

// patchChannelCondition updates a single condition on ch.Status.Conditions
// in-place and calls Status().Update. Idempotent: if the condition is already
// in the desired state the call is still made (status subresource ignores
// no-diff updates gracefully).
func (r *Reconciler) patchChannelCondition(ctx context.Context, ch *spiceboxv1alpha1.Channel, cond metav1.Condition) error {
	conditions.Set(ch, &ch.Status.Conditions, cond)
	if err := r.Client.Status().Update(ctx, ch); err != nil {
		return fmt.Errorf("patchChannelCondition %s: %w", cond.Type, err)
	}
	return nil
}
