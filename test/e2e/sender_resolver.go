//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"sync"

	"github.com/nats-io/nats.go"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink/viewlink"
)

// e2eSenderResolver is the in-process equivalent of
// internal/cmd/channelsd/sender_resolver.go's senderResolver. It satisfies
// outbound.SenderResolver with the minimum the harness's outbound relay
// needs: a per-Channel default Sender (used by KindUserMessage,
// KindPlanUpdate, etc.) and a per-(Channel, sub-channel) Sender (used by
// KindPermissionRequest, KindToolApproval*). StreamDeltaSink is left as
// "opt out" — the fake kind already returns nil for it, and no e2e test
// asserts on live stream rendering today.
//
// Senders are cached per Channel to match the production resolver's
// shape; the cache also matters because some kinds (slack) own per-thread
// state on the sender, and a fresh sender on every event would erase it.
type e2eSenderResolver struct {
	cli client.Client
	nc  *nats.Conn

	mu    sync.Mutex
	cache map[string]channelkinds.Sender // key = "ns/channelName" or "ns/channelName|subName"
}

func newE2ESenderResolver(cli client.Client, nc *nats.Conn) *e2eSenderResolver {
	return &e2eSenderResolver{
		cli:   cli,
		nc:    nc,
		cache: map[string]channelkinds.Sender{},
	}
}

func (r *e2eSenderResolver) resolveDeps(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.Kind, channelkinds.Deps, error) {
	ch, sec, k, err := resolve.ForSession(ctx, r.cli, sess)
	if err != nil {
		return nil, channelkinds.Deps{}, err
	}
	nc := r.nc
	deps := channelkinds.Deps{
		Channel:     ch,
		Secret:      sec,
		NATSPublish: func(subj string, p []byte) error { return nc.Publish(subj, p) },
		Inbound:     nil, // Senders never call back into the inbound pipeline.
		K8sClient:   r.cli,
		// Mirrors internal/cmd/channelsd/main.go's newArtifactViewMinter. A
		// kind's live_view_offer sender treats a nil minter as "webd is not
		// configured on this install" and posts NOTHING — so leaving it unset
		// made an artifact_offer_view call return "live-view offer sent" to the
		// model while no surface ever showed the offer, which is precisely the
		// gap a scenario asserting on the tool result alone cannot see.
		ArtifactViewMinter: harnessArtifactViewMinter(),
	}
	return k, deps, nil
}

// harnessArtifactViewMinter is the harness's stand-in for the channelsd link
// minter: a real viewlink.Minter over a fixed key and base URL.
//
// A real one rather than a stub, because the value it produces is a signed
// capability — a scenario that later drives the click path has to get a link
// webd would actually verify, and a stub returning a plausible string would
// pass here and fail there. The key is fixed and public by construction; it
// signs nothing that leaves the test process.
func harnessArtifactViewMinter() channelkinds.ArtifactViewMinter {
	return &viewlink.Minter{
		Signer: passthroughlink.New([]byte("e2e-harness-passthroughlink-key-32b"),
			passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd)),
		WebdBaseURL: func() string { return "https://webd.example.invalid" },
	}
}

// SenderFor returns the default-sub-channel Sender for the session's
// Channel. The fake kind's default sender appends each KindUserMessage
// envelope to Driver.sent, which is exactly what ExpectAgentReply polls.
func (r *e2eSenderResolver) SenderFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	if sess.Spec.InputChannel == nil {
		return nil, fmt.Errorf("session %s/%s is not channel-attached", sess.Namespace, sess.Name)
	}
	key := sess.Namespace + "/" + sess.Spec.InputChannel.Name
	r.mu.Lock()
	if s, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return s, nil
	}
	r.mu.Unlock()

	k, deps, err := r.resolveDeps(ctx, sess)
	if err != nil {
		return nil, err
	}
	sender := k.NewSender(deps)
	r.mu.Lock()
	r.cache[key] = sender
	r.mu.Unlock()
	return sender, nil
}

// SubChannelSenderFor returns the kind's named-sub-channel Sender (e.g.
// "permission_request", "tool_approval"). The fake kind only implements
// the "message" sub-channel; for everything else the kind returns nil and
// the relay drops the envelope silently. Tests that need a richer fake —
// e.g. asserting on the rendered Approve/Deny prompt — should add the
// missing sub-channel handler in pkg/channels/channelkinds/fake rather than
// reimplementing one here.
func (r *e2eSenderResolver) SubChannelSenderFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, name string) (channelkinds.Sender, error) {
	if sess.Spec.InputChannel == nil {
		return nil, fmt.Errorf("session %s/%s is not channel-attached", sess.Namespace, sess.Name)
	}
	key := sess.Namespace + "/" + sess.Spec.InputChannel.Name + "|" + name
	r.mu.Lock()
	if s, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return s, nil
	}
	r.mu.Unlock()

	k, deps, err := r.resolveDeps(ctx, sess)
	if err != nil {
		return nil, err
	}
	sender := k.SubChannelSender(name, deps)
	r.mu.Lock()
	r.cache[key] = sender // may be nil; cache it so we don't re-hit K8s
	r.mu.Unlock()
	return sender, nil
}

// StreamDeltaSinkFor: the fake kind opts out of stream rendering by
// returning nil from NewStreamDeltaSink. Returning (nil, nil) here makes
// the outbound relay drop KindAssistantStreamDelta envelopes silently —
// exactly the production behavior on a fake channel.
func (r *e2eSenderResolver) StreamDeltaSinkFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error) {
	return nil, nil
}
