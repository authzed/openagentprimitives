package pipeline

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// resolveSessionOwnership works out who owns a freshly-created session, so the
// channel can state it in the message that announces the session.
//
// It calls the SAME resolver the operator uses to write agentsession#owner.
// That sharing is the point: the precedence is subtle in a way that makes
// guessing dangerous. A starting user outranks BOTH ownerless sources, so a
// channel configured with ownerless.fromOutputChannel yields an INDIVIDUAL
// owner on a kind that attributes messages to a user (Slack: the person who
// summoned the agent) and a whole-channel owner only on a kind that does not.
// A disclosure derived from the config flag alone would confidently tell a room
// "everyone here owns me" while the operator had written one person as owner —
// a false statement about authority, which is worse than saying nothing.
//
// Best-effort by design. The operator resolves authoritatively and fails the
// session if it cannot; this call only decides what to SAY, so an unresolvable
// owner yields the zero value (no claim made) rather than an error.
func resolveSessionOwnership(
	ctx context.Context,
	c client.Reader,
	sess *spiceboxv1alpha1.AgentSession,
	inputCh *spiceboxv1alpha1.Channel,
	class *spiceboxv1alpha1.AgentClass,
) channelkinds.SessionOwnership {
	var policy *spiceboxv1alpha1.ChannelOwnerPolicy
	if inputCh != nil {
		policy = inputCh.Spec.Owner
	}

	starter := spiceboxv1alpha1.StartedBySubject(sess).String()
	// Two passes, mirroring the operator's own resolver: everything that
	// outranks the output channel's membership — passthrough, an explicit
	// owner, a starting user, a declared ownerless permission — is decided
	// without a group ref at all, and only a session that resolved to nothing
	// pays for the API read. The precedence stays in ResolveOwnerSubject alone.
	ceiling := class.Spec.GetOwnerCeiling()
	subject, source, err := spiceboxv1alpha1.ResolveOwnerSubject(class.Spec.IdentityMode, policy, starter, "", ceiling)
	if err != nil {
		if ref := outputChannelGroupRef(ctx, c, sess, inputCh, class); ref != "" {
			subject, source, err = spiceboxv1alpha1.ResolveOwnerSubject(class.Spec.IdentityMode, policy, starter, ref, ceiling)
		}
	}
	if err != nil {
		// Not an error path for the inbound: the operator is the authority on
		// whether this session may run. Logged rather than dropped so a
		// disagreement between the two is diagnosable.
		log.FromContext(ctx).Info("session ownership not resolvable for disclosure; no claim will be made",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return channelkinds.SessionOwnership{}
	}
	return channelkinds.SessionOwnership{
		Subject:    subject,
		Collective: spiceboxv1alpha1.IsCollectiveOwnership(source),
	}
}

// outputChannelGroupRef resolves the OUTPUT channel and asks its kind for the
// membership subject-set. Mirrors the operator's own lookup; the kind-facing
// half is shared via chregistry.OwnerGroupRefForChannel.
//
// Asked unconditionally of the Channel rather than gated on
// spec.owner.ownerless.fromOutputChannel, for the reason the operator gives at
// pkg/controllers/agentsession/owner_resolve.go: a Channel that declared nothing
// at all is exactly the case the derivation exists for, and by the time the
// caller reaches here every declared owner source has already won. Gating on the
// flag instead left a DERIVED owner undisclosed — the operator wrote a
// whole-channel owner and the room announcing the session was told nothing.
//
// What IS gated is the class's ownerCeiling — the admin veto ResolveOwnerSubject
// never sees, and which a derived owner (never written into a Channel's spec)
// would otherwise slip past entirely. Honouring it here is what keeps the
// disclosure from claiming an owner the operator refused to write.
func outputChannelGroupRef(
	ctx context.Context,
	c client.Reader,
	sess *spiceboxv1alpha1.AgentSession,
	inputCh *spiceboxv1alpha1.Channel,
	class *spiceboxv1alpha1.AgentClass,
) string {
	if !spiceboxv1alpha1.OwnerMayComeFromOutputChannel(class.Spec.GetOwnerCeiling()) {
		return ""
	}
	outCh := inputCh // OutputChannel nil ⇒ the same channel is both input and output
	if sess.Spec.OutputChannel != nil {
		var ch spiceboxv1alpha1.Channel
		if err := c.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.OutputChannel.Name}, &ch); err == nil {
			outCh = &ch
		} else {
			log.FromContext(ctx).Info("ownership disclosure: output channel unreadable; falling back to the input channel",
				"session", sess.Namespace+"/"+sess.Name,
				"outputChannel", sess.Spec.OutputChannel.Name, "err", err.Error())
		}
	}
	return chregistry.OwnerGroupRefForChannel(outCh)
}
