package channelevents

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// KindRestartTrigger is published by channelsd's per-channel-kind restart
// handler when a user completes a "Restart from here" action. Consumed by the
// AgentSession controller's restart reconciler.
const KindRestartTrigger Kind = "restart_trigger"

// RestartTriggerPayload mirrors v1alpha1.PendingRestart at the wire
// level. Field names match for easy round-trip into the CRD.
type RestartTriggerPayload struct {
	// CutTurnIndex is the turn the user clicked Restart on. Memory
	// turns 0..CutTurnIndex (inclusive) will be copied to the child
	// session; the user's edit becomes turn CutTurnIndex+1.
	CutTurnIndex int32 `json:"cutTurnIndex"`

	// NewUserText is the message text the user submitted through
	// the channel-kind modal.
	NewUserText string `json:"newUserText"`

	// TriggeredBy is the canonical subject of the user (e.g.,
	// "user:alice"). Used for interact-perm re-check.
	TriggeredBy identity.Subject `json:"triggeredBy"`

	// TargetSessionName is the deterministic name of the child
	// AgentSession the reconciler will create. channelsd computes
	// it so a duplicate trigger from a second replica resolves to
	// the same name.
	TargetSessionName string `json:"targetSessionName"`

	// KindRequestRef is the channel-kind-opaque round-trip handle
	// (e.g., Slack response_url + view_id) that the kind's
	// continuation messaging will use to ack back to the user.
	// Treated as opaque by the operator.
	KindRequestRef string `json:"kindRequestRef,omitempty"`
}

// Validate returns nil if the payload is well-formed.
func (p RestartTriggerPayload) Validate() error {
	if p.CutTurnIndex < 0 {
		return fmt.Errorf("RestartTriggerPayload: CutTurnIndex must be >= 0 (got %d)", p.CutTurnIndex)
	}
	if p.NewUserText == "" {
		return fmt.Errorf("RestartTriggerPayload: NewUserText must be non-empty")
	}
	if p.TriggeredBy == "" {
		return fmt.Errorf("RestartTriggerPayload: TriggeredBy must be non-empty")
	}
	if p.TargetSessionName == "" {
		return fmt.Errorf("RestartTriggerPayload: TargetSessionName must be non-empty")
	}
	return nil
}
