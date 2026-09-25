// Package resolve loads the bound Channel CR, its credentials Secret,
// and the registered channelkinds.Kind for a channel-attached
// AgentSession. Used by channelsd's outbound sender + inbound listener
// startup paths AND by the runner's lookup_user_for_mention setup, so
// the loading logic lives in exactly one place.
package resolve

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// ForSession loads the bound Channel CR, its credentials Secret, and
// the registered Kind for a channel-attached AgentSession. Returns
// wrapped errors that name which step failed:
//
//   - "session %q is not channel-attached" — both InputChannel and OutputChannel are nil.
//   - "get channel: ..."                    — Channel CR lookup failed.
//   - "get secret: ..."                     — credentials Secret lookup failed.
//   - "unknown kind %q"                      — kind name not in the registry.
//
// OutputChannel wins over InputChannel when both are set — that is the
// split-channel case, a cron-driven agent with a dedicated output channel.
func ForSession(
	ctx context.Context,
	cli client.Client,
	sess *spiceboxv1alpha1.AgentSession,
) (*spiceboxv1alpha1.Channel, *corev1.Secret, channelkinds.Kind, error) {
	if sess == nil {
		return nil, nil, nil, fmt.Errorf("ForSession: nil AgentSession")
	}
	// Prefer OutputChannel (split-channel sessions); fall back to InputChannel.
	binding := sess.Spec.OutputChannel
	if binding == nil {
		binding = sess.Spec.InputChannel
	}
	if binding == nil {
		return nil, nil, nil, fmt.Errorf("session %q is not channel-attached", sess.Namespace+"/"+sess.Name)
	}
	var ch spiceboxv1alpha1.Channel
	if err := cli.Get(ctx, types.NamespacedName{
		Namespace: sess.Namespace, Name: binding.Name,
	}, &ch); err != nil {
		return nil, nil, nil, fmt.Errorf("get channel: %w", err)
	}
	sec, k, err := ForChannel(ctx, cli, &ch)
	if err != nil {
		return nil, nil, nil, err
	}
	return &ch, sec, k, nil
}

// ForChannel loads the credentials Secret and looks up the registered
// Kind for an already-resolved Channel. Used where the Channel is the
// trigger input (channelsd listener startup) and reused by ForSession.
//
// A Channel that references no Secret resolves to an EMPTY one rather than an
// error. spec.credentialsRef.secretName is a required CRD field but carries no
// MinLength, and the Channel reconciler has always skipped its Secret checks on
// an empty value ("some future kinds may have no creds" —
// pkg/controllers/channel/controller.go). This is the same rule, one layer
// down: without it a kind=agent Channel — whose counterparty is another
// AgentSession in this cluster, so there is nothing to hold a credential for —
// failed here with `get secret: secrets "" not found`, which took down every
// caller. channelsd's senderResolver is the one that mattered: no Sender means
// the outbound relay drops the envelope, so a conversational subagent could
// never answer its parent.
//
// EMPTY, not nil: Deps.Secret is dereferenced by kinds that read data keys, and
// the credential-less kinds already in production (local, browser, bento) point
// at a Secret that exists and holds nothing. Returning the zero Secret makes a
// Channel with no secretName behave exactly like those, instead of adding a nil
// every consumer would have to learn about.
func ForChannel(
	ctx context.Context,
	cli client.Client,
	ch *spiceboxv1alpha1.Channel,
) (*corev1.Secret, channelkinds.Kind, error) {
	if ch == nil {
		return nil, nil, fmt.Errorf("ForChannel: nil channel")
	}
	var sec corev1.Secret
	if name := ch.Spec.CredentialsRef.SecretName; name != "" {
		if err := cli.Get(ctx, types.NamespacedName{
			Namespace: ch.Namespace, Name: name,
		}, &sec); err != nil {
			return nil, nil, fmt.Errorf("get secret: %w", err)
		}
	}
	k, ok := registry.Get(ch.Spec.Kind)
	if !ok {
		return nil, nil, fmt.Errorf("unknown kind %q", ch.Spec.Kind)
	}
	return &sec, k, nil
}
