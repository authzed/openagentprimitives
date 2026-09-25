// Package triggeredidle registers the idle-status gate that keeps a triggered,
// humanless session's pinned status from reading "in progress" forever.
//
// A session started by a trigger with a status surface (a GitHub PR check run)
// and NO human starter has no conversation to keep alive: once its turn ends it
// parks Idle, wakeable only by a later inbound to the same trigger (a second
// push, a redelivery). That Idle park is correct and must stay — but the pinned
// opening badge would otherwise show "in progress" indefinitely. This gate
// supplies a terminal badge for that case, on the STATUS axis only; the session
// stays Idle and re-joinable.
package triggeredidle

import (
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pinnededit"
)

func init() { pinnededit.Register(gate{}) }

type gate struct{}

func (gate) Key() string { return "triggered-session-idle-concluded" }

func (gate) TerminalBadgeForIdle(sess *v1alpha1.AgentSession) (v1alpha1.OpeningBadge, bool) {
	if sess == nil || sess.Spec.InputChannel == nil {
		return "", false
	}
	// Only a trigger with a status surface (a GitHub PR check run) has an
	// "in progress" that strands — a conversational kind has a person who may
	// speak again, so its Idle is a genuine wait, not a wedge.
	if _, ok := chregistry.TriggerStatusReporterFor(sess.Spec.InputChannel.Kind); !ok {
		return "", false
	}
	// A human starter means a real conversation to keep alive; leave its Idle
	// semantics untouched. Only the humanless, trigger-driven session — nothing
	// will ever wake it except the trigger itself — must not read "in progress".
	if !v1alpha1.StartedByCanonical(sess).IsZero() {
		return "", false
	}
	return v1alpha1.OpeningBadgeDone, true
}
