// pkg/channels/channelsd/pipeline/monitoring_recipient.go
//
// monitoringRecipientExists / monitoringChannelUndeliverable answer the ONE
// question every watcher that fans a MonitoringEvent out to role=monitoring
// Channels needs answered before it may claim delivery: is there a Channel
// that can ACTUALLY receive one. Originally CredentialUpdateWatcher methods
// (credential_update.go); extracted to free functions so WorkshopHandoffWatcher
// (workshop_handoff.go) can share the same fail-closed check without either
// watcher depending on the other's type.
package pipeline

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// monitoringRecipientExists reports whether any role=monitoring Channel can
// ACTUALLY receive a MonitoringEvent, plus a human-readable reason when none
// can.
//
// EXISTENCE IS NOT DELIVERABILITY, and conflating them is the worst outcome
// here. PublishMonitoring writes to a fixed NATS subject and returns nil whether
// or not the relay finds anyone to hand the event to, since the relay drops it
// per-Channel. A pre-check asking only "does a role=monitoring Channel exist"
// would therefore count a delivery for a Channel whose kind cannot deliver
// monitoring at all, whose spec is unusable for it, or whose Secret lacks the
// keys the kind needs — stamping a delivered-card status for a card nobody
// received, DISARMING the caller's "nobody was ever asked" branch. That is the
// precise falsehood callers rely on this to prevent.
//
// So it asks the relay's own four questions, every one through the kind's
// registry-declared contract — no `if kind == "slack"` anywhere:
//
//   - registry.Get: an unregistered kind has no sender to build.
//   - SupportsMonitoring: the kind's declaration, shared with the relay, so the
//     two agree BY CONSTRUCTION rather than by staying in sync.
//   - ValidateSpec: the kind's own spec check.
//   - RequiredSecretKeys against the live Secret: what its sender needs to
//     authenticate, which otherwise errors only at send time.
//
// Any Channel passing all four is a genuine recipient. That Secret read is why
// this is not merely a List — see each caller's own cost note.
func monitoringRecipientExists(ctx context.Context, k8s client.Client) (bool, string, error) {
	var list spiceboxv1alpha1.ChannelList
	if err := k8s.List(ctx, &list); err != nil {
		return false, "", fmt.Errorf("list Channels: %w", err)
	}
	var rejected []string
	for i := range list.Items {
		ch := &list.Items[i]
		if ch.Spec.Role != spiceboxv1alpha1.ChannelRoleMonitoring {
			continue
		}
		if why := monitoringChannelUndeliverable(ctx, k8s, ch); why != "" {
			rejected = append(rejected, ch.Namespace+"/"+ch.Name+" ("+why+")")
			continue
		}
		return true, "", nil
	}
	if len(rejected) > 0 {
		return false, "every Channel with role=monitoring is undeliverable: " + strings.Join(rejected, "; "), nil
	}
	return false, "no Channel with role=monitoring exists to broadcast to", nil
}

// monitoringChannelUndeliverable returns "" when ch is a genuine monitoring
// recipient, or a short reason why it is not. Split out so each rejection
// carries its own cause into the caller's condition/log instead of collapsing
// to a single boolean -- an operator who configured a monitoring Channel and
// still sees "no recipient" needs to know WHICH of its four requirements it
// misses.
func monitoringChannelUndeliverable(ctx context.Context, k8s client.Client, ch *spiceboxv1alpha1.Channel) string {
	k, ok := registry.Get(ch.Spec.Kind)
	if !ok {
		return fmt.Sprintf("kind %q is not registered in this binary", ch.Spec.Kind)
	}
	if !k.SupportsMonitoring() {
		return fmt.Sprintf("kind %q does not deliver monitoring events", ch.Spec.Kind)
	}
	if err := k.ValidateSpec(ch); err != nil {
		return "spec is invalid for its kind: " + err.Error()
	}
	// The Secret is read whether or not the kind declares required keys, because
	// the relay's resolve.ForChannel Gets it UNCONDITIONALLY and drops the
	// Channel when that Get fails — it has no RequiredSecretKeys short-circuit.
	// Adding one here would judge deliverable a Channel the relay then silently
	// drops, stamping a delivered card for one nobody received. Reachable with
	// kind "fake" (nil RequiredSecretKeys) pointed at a missing Secret.
	var sec corev1.Secret
	key := client.ObjectKey{Namespace: ch.Namespace, Name: ch.Spec.CredentialsRef.SecretName}
	if err := k8s.Get(ctx, key, &sec); err != nil {
		return "credentials Secret " + key.String() + " is unreadable: " + err.Error()
	}
	for _, want := range k.RequiredSecretKeys(ch) {
		if len(sec.Data[want]) == 0 {
			return fmt.Sprintf("credentials Secret %s is missing the %q key its kind requires", key.String(), want)
		}
	}
	return ""
}
