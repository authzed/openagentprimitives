package local

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

// localSender is the host-side main Sender: KindUserMessage,
// KindNotification, KindPlanUpdate → render events. Translation only;
// all layout lives in the bubbletea model.
type localSender struct {
	sink *inertSink
}

// refOf maps a SessionInfo to the render-event SessionRef.
func refOf(sess channelkinds.SessionInfo) SessionRef {
	return SessionRef{Namespace: sess.Namespace, Name: sess.Name}
}

// emitSendError emits a visible MsgSendError and returns a wrapped
// error. Both surfacing paths fire — the user sees a timeline warning
// AND the relay/caller gets the error (AGENTS.md: never silently drop).
func (s *localSender) emitSendError(sess channelkinds.SessionInfo, k channelevents.Kind, err error) (channelkinds.SubChannelSendResult, error) {
	s.sink.Emit(MsgSendError{
		Session: refOf(sess), Kind: k, Err: err.Error(), At: timeNowUTC(),
	})
	return channelkinds.SubChannelSendResult{}, fmt.Errorf("local sender (%s): %w", k, err)
}

func (s *localSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindUserMessage:
		var pl channelevents.OutboundUserMessagePayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitSendError(sess, env.Kind, err)
		}
		// The agent's own reply, and the least filtered text this kind
		// carries: everything else is composed by the runner around the
		// model's output, this IS the model's output. Like every other event
		// here it is made inert by the sink itself (inert_sink.go); no sender
		// in this package sweeps its own fields, which is what stopped one of
		// them being forgotten.
		s.sink.Emit(MsgUserMessage{Session: refOf(sess), Text: pl.Text})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindNotification:
		var pl channelevents.NotificationPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitSendError(sess, env.Kind, err)
		}
		// update_status text is the agent's own caption for what it is doing,
		// drawn on the surface's status line — one of the few lines that is
		// always visible, including while a decision modal is open. So is the
		// operation snapshot's CompactLine below, which the channelsd watchdog
		// fills from THIS field on a cleared tick: one sink sweeping and the
		// other not is how the same string ended up inert on one route and live
		// on the other.
		s.sink.Emit(MsgNotification{
			Session:                 refOf(sess),
			Text:                    pl.Text,
			Short:                   pl.Short,
			ExpectedDurationSeconds: pl.ExpectedDurationSeconds,
			Ephemeral:               pl.Ephemeral,
		})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindTurnProgress:
		// The runner's progressReporter publishes a throttled cumulative
		// token/elapsed snapshot per turn. Like Slack (which renders it on the
		// assistant status line) and the web chat, the local kind routes it
		// through the main Sender. Without this case every tick of an active
		// turn — one roughly every 5 seconds — hit the default below and painted
		// a warning line into the TUI timeline.
		var pl channelevents.TurnProgressPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitSendError(sess, env.Kind, err)
		}
		s.sink.Emit(MsgTurnProgress{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindToolProgress:
		var pl channelevents.ToolProgressPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitSendError(sess, env.Kind, err)
		}
		s.sink.Emit(MsgToolProgress{
			Session: refOf(sess), CallID: pl.CallID, Name: pl.Name,
			BudgetSeconds: pl.BudgetSeconds, ElapsedSeconds: pl.ElapsedSeconds, Done: pl.Done,
			Percent: pl.Percent, TailLine: pl.TailLine, EtaSeconds: pl.EtaSeconds,
		})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindPlanUpdate:
		var pl channelevents.PlanUpdatePayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitSendError(sess, env.Kind, err)
		}
		s.sink.Emit(MsgPlanUpdate{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindOperationActivity:
		// The runner publishes a throttled snapshot of the active operation
		// subtree; the channelsd relay overwrites CompactLine with its resolved
		// EffectiveLine before this Sender sees it. Route it to the TUI like the
		// Slack/web-chat channels do.
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
