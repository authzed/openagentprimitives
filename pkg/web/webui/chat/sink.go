package chat

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
)

// wsWriteTimeout bounds each websocket write so a stuck client cannot pin the
// writer goroutine forever (mirrors artifactview's liveWriteTimeout).
const wsWriteTimeout = 10 * time.Second

// wsSinkBufferSize bounds one slow tab's pending frames before the sink starts
// dropping the OLDEST to make room for the newest. Sized to absorb a normal
// burst (stream deltas, progress snapshots) without dropping. The sink is
// live-only best-effort — the durable transcript lives in operator memory — so
// dropping frames for a wedged tab is degraded-but-safe, and far better than
// blocking every OTHER session on the shared outbound subscription goroutine.
const wsSinkBufferSize = 256

// defaultWSPingInterval keeps an otherwise-idle chat websocket warm. The ws is
// push-only and can sit silent for minutes during a slow runner cold start or
// a long turn; browsers and proxies drop an idle connection, and because the
// sink has no replay, every frame emitted during that gap — the agent's reply,
// the turn-complete — is lost until a full page reload. 30s stays well under
// the common 60s idle-timeout floor. Overridable per-Registry (Registry.wsPing)
// so tests can shrink it without a shared mutable global.
const defaultWSPingInterval = 30 * time.Second

// emitter is the internal per-connection sink interface sessionEntry fans out
// to. *wsSink is the production implementation; tests use a lightweight fake
// so the registry's attach/detach/broadcast bookkeeping can be exercised
// without a real websocket.
type emitter interface {
	browser.EventSink
	// Close ends the underlying connection so the browser side notices
	// (and, for a websocket, so its own read pump exits and triggers detach).
	Close()
}

// frameEmitter is the optional richer interface a production sink implements to
// take a wire frame that sessionEntry has ALREADY marshaled — so a broadcast to
// N tabs marshals the frame once, not once per tab. sessionEntry.Emit checks
// for it via a type assertion: real websocket sinks receive the pre-marshaled
// bytes; recording fakes fall back to emitter.Emit with the typed event.
type frameEmitter interface {
	emitFrame(frame []byte)
}

// wsSink is the per-connection, websocket-backed emitter. One is created per
// GET /sessions/api/{ns}/{name}/ws upgrade and attached to the session's
// sessionEntry for the connection's lifetime.
//
// Delivery is decoupled from the caller: Emit/emitFrame are non-blocking
// enqueues onto a bounded buffered channel, and a single dedicated writer
// goroutine (started in newWSSink) owns EVERY conn write. That is the point of
// the design — the outbound relay fans every session's frames from ONE NATS
// subscription goroutine, so a synchronous write path lets one slow tab
// (holding the write mutex across a 10s deadline) block delivery to every
// OTHER session in the process.
type wsSink struct {
	conn   *websocket.Conn
	logger logr.Logger
	ref    browser.SessionRef

	// frames is the bounded delivery buffer the writer goroutine drains. It is
	// closed exactly once (under mu, guarded by dead) so the writer drains any
	// buffered frames and then exits — no goroutine leak, no send-on-closed
	// panic.
	frames chan []byte

	mu sync.Mutex
	// dead is set once the sink is finished (a failed write / ping, or Close).
	// It guards both further enqueues (they become no-ops) and the single
	// close(frames), so the close can never happen twice or race an in-flight
	// enqueue.
	dead bool
	// slowWarned dedups the "sink slow; dropping frames" log to once per slow
	// episode (reset on the next unblocked enqueue), so a wedged tab can't
	// flood the logs with one line per dropped frame.
	slowWarned bool
}

// newWSSink builds a websocket sink and starts its dedicated writer goroutine.
// The goroutine terminates (no leak) the moment the sink is closed — via
// Close() on teardown / detach, or via a failed write / ping — because every
// termination path closes the frames channel exactly once.
func newWSSink(conn *websocket.Conn, logger logr.Logger, ref browser.SessionRef) *wsSink {
	s := &wsSink{
		conn:   conn,
		logger: logger,
		ref:    ref,
		frames: make(chan []byte, wsSinkBufferSize),
	}
	go s.writeLoop()
	return s
}

// Emit implements browser.EventSink for the teardown path (and any caller that
// only holds the typed event): it marshals the event to a wire frame and hands
// the bytes to the non-blocking enqueue. The hot broadcast path does NOT go
// through here — sessionEntry.Emit marshals once and calls emitFrame directly.
// An unrecognized event type is logged and dropped (never panics on an unknown
// shape), per the project's never-silently-drop-errors rule.
func (s *wsSink) Emit(msg any) {
	frame, ok := toFrame(msg)
	if !ok {
		s.logger.Info("chat ws sink: dropping unrecognized event type", "session", s.ref)
		return
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		s.logger.Info("chat ws sink: marshal frame failed; dropping", "session", s.ref, "type", frame.Type, "err", err.Error())
		return
	}
	s.emitFrame(raw)
}

// emitFrame is the non-blocking enqueue: it never blocks the caller (the
// shared outbound goroutine). On a full buffer it drops the OLDEST pending
// frame to make room for the newest, warning once per slow episode; after the
// sink is dead it is a safe no-op. Holding mu across the non-blocking channel
// ops is deliberate — it is the same lock close(frames) is taken under, so an
// enqueue can never send on a channel Close is concurrently closing.
func (s *wsSink) emitFrame(frame []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	select {
	case s.frames <- frame:
		s.slowWarned = false // an unblocked send ends any slow episode
	default:
		// Buffer full: a slow / back-pressured client. Drop the oldest buffered
		// frame so this newer one takes its place (drop-oldest). Both ops are
		// non-blocking; after dropping one there is a free slot, so the send
		// below succeeds (the inner default is a belt-and-suspenders no-op).
		select {
		case <-s.frames:
		default:
		}
		select {
		case s.frames <- frame:
		default:
		}
		if !s.slowWarned {
			s.slowWarned = true
			s.logger.Info("chat ws sink: slow; dropping frames", "session", s.ref)
		}
	}
}

// writeLoop is the sink's single writer goroutine: it owns every conn write.
// It drains the frames channel until the channel is closed (graceful Close:
// drains all buffered frames, incl a final session_ended, then exits) or a
// write fails (marks the sink dead, closes the channel, exits). Its deferred
// conn.Close() unblocks the handler's read pump so the connection is detached.
func (s *wsSink) writeLoop() {
	defer func() { _ = s.conn.Close() }()
	for frame := range s.frames {
		if err := s.writeFrame(frame); err != nil {
			s.logger.Info("chat ws sink: write failed; closing connection", "session", s.ref, "err", err.Error())
			s.shutdownSink()
			return
		}
	}
}

// writeFrame writes one already-marshaled frame under the write deadline. All
// calls come from the single writer goroutine, satisfying gorilla's
// one-writer-at-a-time contract for WriteMessage / SetWriteDeadline (ping's
// WriteControl and Close are explicitly exempt and safe concurrently).
func (s *wsSink) writeFrame(frame []byte) error {
	if err := s.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.TextMessage, frame)
}

// shutdownSink marks the sink dead and closes the frames channel exactly once,
// so the writer goroutine drains what's buffered and then exits (its deferred
// conn.Close closes the connection). Idempotent, and safe against a concurrent
// enqueue: both take mu, so the close can never race an in-flight send.
func (s *wsSink) shutdownSink() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	s.dead = true
	close(s.frames)
}

// ping writes a WebSocket ping control frame to keep an idle connection warm
// and probe for a half-open peer (the handler's read pump expects the matching
// pong within its read deadline). Returns false once the sink is dead, so the
// keepalive loop stops. gorilla permits WriteControl concurrently with the
// writer goroutine's WriteMessage, so ping does NOT hold mu across the write:
// holding it would let a stuck 10s ping block every enqueue, reintroducing the
// head-of-line blocking this design removes. A ping failure shuts the sink
// down and is logged, never swallowed.
func (s *wsSink) ping() bool {
	s.mu.Lock()
	dead := s.dead
	s.mu.Unlock()
	if dead {
		return false
	}
	if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout)); err != nil {
		s.logger.Info("chat ws sink: keepalive ping failed; closing connection", "session", s.ref, "err", err.Error())
		s.shutdownSink()
		return false
	}
	return true
}

// Close ends the sink gracefully: it closes the frames channel so the writer
// goroutine flushes any buffered frames (e.g. a session_ended enqueued just
// before Close) and then closes the connection. It never blocks on a slow
// client — closing a channel is instant — so teardown / shutdown can fan Close
// across many sinks without stalling on a stuck tab.
func (s *wsSink) Close() {
	s.shutdownSink()
}

var (
	_ emitter      = (*wsSink)(nil)
	_ frameEmitter = (*wsSink)(nil)
)

// wsFrame is the JSON envelope pushed to the browser over the chat
// websocket: {"type": "<discriminator>", "session": {...}, "payload": {...}}.
type wsFrame struct {
	// Type is the client's render discriminator; see toFrame for the full set.
	Type string `json:"type"`
	// Session is the conversation this frame belongs to, so a tab can ignore
	// anything not its own.
	Session browser.SessionRef `json:"session"`
	// Payload is the typed render event; omitted when the type alone suffices.
	Payload any `json:"payload,omitempty"`
}

// sessionEndedMsg is emitted (by the registry, not a browser sender) when a
// session is torn down — terminal phase or idle-timeout reaping — so any
// attached tab can show a clear "this conversation has ended" state instead
// of a silently-dead socket.
type sessionEndedMsg struct {
	// Session is the conversation that ended.
	Session browser.SessionRef `json:"session"`
	// Reason is why it ended, and decides which end state the tab renders.
	Reason string `json:"reason"` // "succeeded" | "failed" | "idle_timeout" | "server_shutdown"
	// FailureReason is the machine-readable cause; empty unless Reason=="failed".
	FailureReason string `json:"failureReason,omitempty"`
	// FailureMessage is the human-readable cause; empty unless Reason=="failed".
	FailureMessage string `json:"failureMessage,omitempty"`
}

// sessionStartupMsg is emitted by the health watcher (not a browser sender)
// on every tick a session has not started yet: what is in the way, in the
// words channelsd puts on the session's channel thread, and whether the wait
// has outlived the short startup grace. The final one carries Started=true,
// once, on the tick the session first reaches a started phase — it is what
// takes the shell's startup line down.
//
// Emitted every tick rather than on change: the browser fold returns the same
// object for an unchanged frame (no re-render), and a tab that reconnects
// mid-startup gets the line back on the next tick without a replay.
type sessionStartupMsg struct {
	Session browser.SessionRef `json:"session"`
	// Text is the caption — the generic startup lead when nothing is blocking.
	Text string `json:"text"`
	// Short is the narrow-surface variant; empty means use Text.
	Short string `json:"short,omitempty"`
	// StillTrying is true once a runner-side blocker has outlived startupGrace.
	StillTrying bool `json:"stillTrying,omitempty"`
	// Started is true on the one frame that says the wait is over.
	Started bool `json:"started,omitempty"`
}

// toFrame maps a browser.Msg* render event (or chat's own sessionEndedMsg)
// to its wire frame. ok is false for any other type — callers log and drop
// rather than emit a malformed frame.
func toFrame(msg any) (wsFrame, bool) {
	switch m := msg.(type) {
	case browser.MsgSessionOpening:
		return wsFrame{Type: "session_opening", Session: m.Session, Payload: m}, true
	case browser.MsgUserMessage:
		return wsFrame{Type: "user_message", Session: m.Session, Payload: m}, true
	case browser.MsgUserEcho:
		return wsFrame{Type: "user_echo", Session: m.Session, Payload: m}, true
	case browser.MsgNotification:
		return wsFrame{Type: "notification", Session: m.Session, Payload: m}, true
	case browser.MsgPlanUpdate:
		return wsFrame{Type: "plan_update", Session: m.Session, Payload: m}, true
	case browser.MsgToolSessionEvent:
		return wsFrame{Type: "tool_session_event", Session: m.Session, Payload: m}, true
	case browser.MsgToolSessionDelta:
		return wsFrame{Type: "tool_session_delta", Session: m.Session, Payload: m}, true
	case browser.MsgPermissionRequest:
		return wsFrame{Type: "permission_request", Session: m.Session, Payload: m}, true
	case browser.MsgPermissionDecisionApplied:
		return wsFrame{Type: "permission_decision_applied", Session: m.Session, Payload: m}, true
	case browser.MsgStreamDelta:
		return wsFrame{Type: "stream_delta", Session: m.Session, Payload: m}, true
	case browser.MsgTurnProgress:
		return wsFrame{Type: "turn_progress", Session: m.Session, Payload: m}, true
	case browser.MsgToolProgress:
		return wsFrame{Type: "tool_progress", Session: m.Session, Payload: m}, true
	case browser.MsgTurnActivity:
		return wsFrame{Type: "turn_activity", Session: m.Session, Payload: m}, true
	case browser.MsgOperationActivity:
		return wsFrame{Type: "operation_activity", Session: m.Session, Payload: m}, true
	case browser.MsgInterruptApplied:
		return wsFrame{Type: "interrupt_applied", Session: m.Session, Payload: m}, true
	case browser.MsgLiveViewOffer:
		return wsFrame{Type: "live_view_offer", Session: m.Session, Payload: m}, true
	case browser.MsgInteractionRequest:
		return wsFrame{Type: "interaction_request", Session: m.Session, Payload: m}, true
	case browser.MsgInteractionApplied:
		return wsFrame{Type: "interaction_applied", Session: m.Session, Payload: m}, true
	case browser.MsgInteractionRejected:
		return wsFrame{Type: "interaction_decision_rejected", Session: m.Session, Payload: m}, true
	case browser.MsgSendError:
		return wsFrame{Type: "send_error", Session: m.Session, Payload: m}, true
	case sessionStartupMsg:
		return wsFrame{Type: "session_startup", Session: m.Session, Payload: m}, true
	case sessionEndedMsg:
		return wsFrame{Type: "session_ended", Session: m.Session, Payload: m}, true
	default:
		return wsFrame{}, false
	}
}
