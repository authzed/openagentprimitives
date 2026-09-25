// The Channel Deliverable condition: the relay's record of whether the most
// recent real delivery attempt through a Channel landed. Connected (owned by
// the listener manager) answers "does the socket attach"; this answers "does
// a send actually arrive" — the two diverge exactly in the incident this
// exists for, a Slack bot whose token is healthy but who was never invited to
// (or was evicted from) the destination channel, where every agent reply dies
// as a channelsd log line while the Channel looks green everywhere.
// pkg/controllers/monitoring watches the condition, so the first failed
// delivery reaches the monitoring channel instead of only the logs.
package outbound

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// maxDeliveryErrorRunes bounds the condition message. Provider errors are
// short ("not_in_channel"); the cap only defends against a pathological error
// that embeds a payload, so the status object stays readable.
const maxDeliveryErrorRunes = 512

// deliverableOutcome is one delivery attempt's result, in exactly the
// granularity the condition stores: success, or failure plus the error text.
// Comparing outcomes (not just the boolean) means a failure whose error
// CHANGES still rewrites the message — the operator fixing error A deserves
// to see error B — while an unchanged outcome writes nothing.
type deliverableOutcome struct {
	ok      bool
	message string
}

// noteDeliveryOutcome folds one Send result into the Channel's Deliverable
// condition, writing status only when the outcome differs from what is
// already recorded. A broken channel receives every envelope its sessions
// emit, so this must not cost one status write per failed send: the
// per-channel memo short-circuits repeats, and on a memo miss (process start)
// the live condition is compared first so a restart against an unchanged
// outcome writes nothing. The memo is populated only after a successful
// write, so a failed patch is retried on the next outcome rather than
// silently believed. Map growth is bounded by the set of Channel CRs actually
// routed through, which is operator-created and small.
//
// Failure to record is logged and dropped, never allowed to affect the
// delivery path itself: the condition is an observation about sends, and an
// apiserver hiccup while recording one must not break or retry the send.
func (r *Relay) noteDeliveryOutcome(ctx context.Context, ns string, binding *spiceboxv1alpha1.ChannelBinding, sendErr error) {
	if !r.RecordDeliverability || binding == nil || binding.Name == "" {
		return
	}
	desired := deliverableOutcome{ok: sendErr == nil}
	if sendErr != nil {
		msg := sendErr.Error()
		if runes := []rune(msg); len(runes) > maxDeliveryErrorRunes {
			msg = string(runes[:maxDeliveryErrorRunes]) + "…"
		}
		desired.message = msg
	}
	key := ns + "/" + binding.Name

	r.delivMu.Lock()
	prev, known := r.delivOutcomes[key]
	r.delivMu.Unlock()
	if known && prev == desired {
		return
	}

	logger := log.FromContext(ctx)
	var live spiceboxv1alpha1.Channel
	if err := r.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: binding.Name}, &live); err != nil {
		logger.Info("outbound relay: Deliverable condition not recorded, Channel read failed",
			"channel", key, "err", err.Error())
		return
	}
	if !known {
		if cur := conditions.Find(live.Status.Conditions, spiceboxv1alpha1.ChannelConditionDeliverable); cur != nil &&
			(deliverableOutcome{ok: cur.Status == metav1.ConditionTrue, message: cur.Message}) == desired {
			r.rememberDeliveryOutcome(key, desired)
			return
		}
	}

	updated := live.DeepCopy()
	if desired.ok {
		conditions.SetTrue(updated, &updated.Status.Conditions,
			spiceboxv1alpha1.ChannelConditionDeliverable, spiceboxv1alpha1.ReasonChannelDeliverySucceeded)
	} else {
		conditions.SetFalse(updated, &updated.Status.Conditions,
			spiceboxv1alpha1.ChannelConditionDeliverable, spiceboxv1alpha1.ReasonChannelDeliveryFailed, desired.message)
	}
	if err := r.K8s.Status().Patch(ctx, updated, client.MergeFrom(&live)); err != nil {
		logger.Info("outbound relay: patch Deliverable condition failed",
			"channel", key, "err", err.Error())
		return
	}
	r.rememberDeliveryOutcome(key, desired)
}

func (r *Relay) rememberDeliveryOutcome(key string, o deliverableOutcome) {
	r.delivMu.Lock()
	defer r.delivMu.Unlock()
	if r.delivOutcomes == nil {
		r.delivOutcomes = make(map[string]deliverableOutcome)
	}
	r.delivOutcomes[key] = o
}
