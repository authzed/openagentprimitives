package outbound

import (
	"context"
	"errors"
	"maps"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/outputbind"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// recoverBindingChannelID fills in a missing channel_id (and any other anchor
// metadata the binding lacks) on an outbound binding by re-deriving the
// destination from its Channel CR. It exists for the reviewbot incident: a
// session whose reply lands in a role=output Slack channel whose binding never
// carried a channel_id — so the Slack sender refuses with "external.channel_id
// missing on session.spec.channel" and the reply dies into monitoring silence.
// The Channel CR's configured destination (Slack's outputDefaults.channelId)
// IS a stable source of truth, so recovering from it lets the reply start on
// the configured channel instead of failing.
//
// Kind-agnostic by construction: it routes through the same
// OutboundAnchorProvider seam the inbound pipeline uses to seed the binding at
// session creation (outputbind.Anchor), so a Slack (or fake) destination is
// recovered while an input-only kind that provides no anchor (github, bento) is
// left untouched — there is nothing to recover, and its own sender's refusal is
// the correct, unchanged behavior.
//
// It runs ONLY when the binding lacks channel_id, so the steady-state hot path
// (every send on an established thread) pays a single map lookup and returns.
// The kind check comes before the Channel read so an input-only binding — which
// has no channel_id by nature, and would otherwise trigger a Channel Get on
// every single send — costs nothing beyond a registry lookup.
//
// Best-effort: a Channel read failure or an anchor error is logged (except the
// expected "kind provides no anchor", which is not a problem) and the binding is
// returned unchanged, so the send proceeds to its existing path rather than
// being blocked here. Only keys the binding is missing are filled, so an
// established thread_ts is never clobbered by the channel-level default.
//
// Mutates binding.External in place (cloning a nil map first). binding is a
// pointer into the session the relay just loaded — for the common case it is
// spec.outputChannel itself, so the recovered channel_id is also seen by
// openOutboundThread and the write-back path, which read the same pointer.
func (r *Relay) recoverBindingChannelID(ctx context.Context, logger logr.Logger, ns string, binding *spiceboxv1alpha1.ChannelBinding) {
	if binding == nil || binding.Name == "" || binding.External["channel_id"] != "" {
		return
	}
	k, ok := registry.Get(binding.Kind)
	if !ok {
		return
	}
	if _, ok := k.(channelkinds.OutboundAnchorProvider); !ok {
		// Input-only kinds (github, bento) provide no outbound anchor; there is
		// nothing to recover and their sender's own refusal is the right answer.
		return
	}
	var ch spiceboxv1alpha1.Channel
	if err := r.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: binding.Name}, &ch); err != nil {
		logger.Info("outbound relay: channel_id recovery skipped, Channel read failed",
			"channel", ns+"/"+binding.Name, "err", err.Error())
		return
	}
	_, external, err := outputbind.Anchor(&ch)
	if err != nil {
		// ErrKindNoAnchor here means the destination is genuinely unconfigured
		// (e.g. a Slack channel with no outputDefaults.channelId) — the Channel
		// controller already reports that as Valid=False, and the send's own
		// failure path will surface it, so this is not a new log-worthy event.
		if !errors.Is(err, outputbind.ErrKindNoAnchor) {
			logger.Info("outbound relay: channel_id recovery skipped, anchor failed",
				"channel", ns+"/"+binding.Name, "err", err.Error())
		}
		return
	}
	if external["channel_id"] == "" {
		return
	}
	merged := maps.Clone(binding.External)
	if merged == nil {
		merged = map[string]string{}
	}
	for key, val := range external {
		if merged[key] == "" {
			merged[key] = val
		}
	}
	binding.External = merged
	logger.Info("outbound relay: recovered channel_id from the output Channel CR for a binding that lacked one",
		"channel", ns+"/"+binding.Name, "channelID", external["channel_id"])
}
