// The "interaction" sub-channel sender for the fake kind: the generic
// Interaction model's recorder. One sub-channel name routes BOTH the request
// leg (KindInteractionRequest) and the applied leg (KindInteractionApplied),
// mirroring the outbound relay's dispatch. Sender, recorded types and public
// accessors live together here rather than split across kind.go.
package fake

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// recordedInteractionPrompt pairs a KindInteractionRequest payload with the
// session ref it was bound to. Tests drain via Driver.InteractionPrompts().
type recordedInteractionPrompt struct {
	Payload    channelevents.InteractionRequestPayload
	SessionRef channelkinds.SessionInfo
}

// recordedInteractionApplied pairs a KindInteractionApplied payload with the
// session ref it was bound to. Tests drain via Driver.InteractionApplieds().
type recordedInteractionApplied struct {
	Payload    channelevents.InteractionAppliedPayload
	SessionRef channelkinds.SessionInfo
}

// fakeInteractionSender is the "interaction" sub-channel sender for the fake
// kind. It decodes incoming KindInteractionRequest / KindInteractionApplied
// envelopes and appends to the per-channel Driver's interactionPrompts /
// interactionApplieds queues so tests can assert on the generic Interaction
// model's request→decision→applied loop end to end.
type fakeInteractionSender struct{ drv *Driver }

func newInteractionSender(drv *Driver) *fakeInteractionSender {
	return &fakeInteractionSender{drv: drv}
}

func (s *fakeInteractionSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindInteractionRequest:
		var pl channelevents.InteractionRequestPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake interaction sender: decode request: %w", err)
		}
		s.drv.mu.Lock()
		s.drv.interactionPrompts = append(s.drv.interactionPrompts, recordedInteractionPrompt{Payload: pl, SessionRef: sess})
		s.drv.mu.Unlock()
		return channelkinds.SubChannelSendResult{RequestRef: pl.RequestRef}, nil
	case channelevents.KindInteractionApplied:
		var pl channelevents.InteractionAppliedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake interaction sender: decode applied: %w", err)
		}
		s.drv.mu.Lock()
		s.drv.interactionApplieds = append(s.drv.interactionApplieds, recordedInteractionApplied{Payload: pl, SessionRef: sess})
		s.drv.mu.Unlock()
		return channelkinds.SubChannelSendResult{RequestRef: pl.RequestRef}, nil
	default:
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake interaction sender: unsupported envelope kind %q", env.Kind)
	}
}

// Verify the interface assertion at compile time.
var _ channelkinds.Sender = (*fakeInteractionSender)(nil)

// InteractionPrompt is a public projection of one captured
// KindInteractionRequest envelope. Every prompt category — tool approval, info
// leakage, content inspection, identity choice, permission, provider retry,
// credentials, live-view offers, queued-message acks, metaagent scope — funnels
// through this one sender, so tests for any of them read it.
type InteractionPrompt struct {
	Payload    channelevents.InteractionRequestPayload
	SessionRef channelkinds.SessionInfo
}

// InteractionPrompts returns a snapshot of every KindInteractionRequest
// envelope recorded by this Driver's interaction sub-channel sender.
// Append-only; subsequent calls return additional entries as they arrive.
func (d *Driver) InteractionPrompts() []InteractionPrompt {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]InteractionPrompt, len(d.interactionPrompts))
	for i, p := range d.interactionPrompts {
		out[i] = InteractionPrompt{Payload: p.Payload, SessionRef: p.SessionRef}
	}
	return out
}

// InteractionApplied is a public projection of one captured
// KindInteractionApplied envelope. Used by tests asserting that
// HandleInteractionDecision (pkg/channels/channelsd/pipeline) published the resolved
// outcome to the surface after a decision was applied.
type InteractionApplied struct {
	Payload    channelevents.InteractionAppliedPayload
	SessionRef channelkinds.SessionInfo
}

// InteractionApplieds returns a snapshot of every KindInteractionApplied
// envelope recorded by this Driver's interaction sub-channel sender.
// Append-only; subsequent calls return additional entries as they arrive.
func (d *Driver) InteractionApplieds() []InteractionApplied {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]InteractionApplied, len(d.interactionApplieds))
	for i, p := range d.interactionApplieds {
		out[i] = InteractionApplied{Payload: p.Payload, SessionRef: p.SessionRef}
	}
	return out
}
