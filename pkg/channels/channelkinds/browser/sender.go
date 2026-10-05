package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// timeNowUTC is the package clock; a var so tests can read it without
// importing time directly in every file.
func timeNowUTC() time.Time { return time.Now().UTC() }

// browserSender is the host-side main Sender: KindUserMessage,
// KindNotification, KindPlanUpdate → render events. Translation only;
// all layout lives in the browser-side chat UI.
type browserSender struct {
	sink EventSink
}

// refOf maps a SessionInfo to the render-event SessionRef.
func refOf(sess channelkinds.SessionInfo) SessionRef {
	return SessionRef{Namespace: sess.Namespace, Name: sess.Name}
}

// emitSendError emits a visible MsgSendError and returns a wrapped
// error. Both surfacing paths fire — the user sees a timeline warning
// AND the relay/caller gets the error (AGENTS.md: never silently drop).
func (s *browserSender) emitSendError(sess channelkinds.SessionInfo, k channelevents.Kind, err error) (channelkinds.SubChannelSendResult, error) {
	s.sink.Emit(MsgSendError{
		Session: refOf(sess), Kind: k, Err: err.Error(), At: timeNowUTC(),
	})
	return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser sender (%s): %w", k, err)
}

func (s *browserSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindUserMessage:
		var pl channelevents.OutboundUserMessagePayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitSendError(sess, env.Kind, err)
		}
		if pl.Opening != nil {
			s.sink.Emit(MsgSessionOpening{Session: refOf(sess), Opening: pl.Opening})
			return channelkinds.SubChannelSendResult{}, nil
		}
		var atts []MsgAttachment
		for _, a := range pl.Attachments {
			// Skip an attachment with no artifact id — the browser chip needs it
			// to build the /artifact-download link; without it there is nothing
			// clickable to render.
			if a.ArtifactID == "" {
				continue
			}
			atts = append(atts, MsgAttachment{ArtifactID: a.ArtifactID, Filename: a.Filename, MIME: a.MIME})
		}
		s.sink.Emit(MsgUserMessage{Session: refOf(sess), Text: pl.Text, Attachments: atts, Delivery: pl.Delivery})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindNotification:
		var pl channelevents.NotificationPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitSendError(sess, env.Kind, err)
		}
		s.sink.Emit(MsgNotification{
			Session:                 refOf(sess),
			Text:                    pl.Text,
			Short:                   pl.Short,
			ExpectedDurationSeconds: pl.ExpectedDurationSeconds,
			Ephemeral:               pl.Ephemeral,
		})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindPlanUpdate:
		var pl channelevents.PlanUpdatePayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitSendError(sess, env.Kind, err)
		}
		s.sink.Emit(MsgPlanUpdate{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindTurnProgress:
		// The runner's progressReporter publishes a throttled cumulative
		// token/elapsed snapshot per turn. Like Slack (which renders it on the
		// assistant status line), the browser kind routes it through the main
		// Sender — the browser page turns it into a live status line. The
		// outbound relay's turn-progress watchdog gate is a no-op here (the chat
		// Registry wires no ApplyStatusEvent), so it falls straight through to
		// this Sender.
		var pl channelevents.TurnProgressPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitSendError(sess, env.Kind, err)
		}
		s.sink.Emit(MsgTurnProgress{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindToolProgress:
		// A long SYNC tool the LLM is blocked on; the sandbox publishes a
		// throttled per-tool snapshot. It must be routed rather than left to
		// the default arm below, which would surface every snapshot of a
		// long-running tool as a repeating send_error in the chat.
		var pl channelevents.ToolProgressPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitSendError(sess, env.Kind, err)
		}
		s.sink.Emit(MsgToolProgress{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindOperationActivity:
		// The runner publishes a throttled snapshot of the active operation
		// subtree; the channelsd relay overwrites CompactLine with its resolved
		// EffectiveLine before this Sender sees it. Route it to the browser chat
		// UI like the Slack/local channels do.
		var pl channelevents.OperationActivityPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitSendError(sess, env.Kind, err)
		}
		s.sink.Emit(MsgOperationActivity{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	default:
		return s.emitSendError(sess, env.Kind,
			fmt.Errorf("unsupported envelope kind for the main sender"))
	}
}
