package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/credential"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/toolorigin"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
)

type natsRuntime struct {
	conn      *nats.Conn
	inboundCh chan struct{}
	sub       *nats.Subscription
}

// inboundWakeSubject returns the ONE subject an await_user_message wake-up may
// arrive on: <prefix>.in.user_message, published by channelsd's
// Pipeline.publishWakeup once the new user turn is in the session's inbox.
//
// It must never widen to a `<prefix>.in.>` wildcard. A token on inboundCh means
// exactly "a new user turn exists" to its only consumer. The runner publishes
// its OWN envelopes (interaction_request, interaction_applied,
// metaagent_request) onto this same namespace over this same connection, and
// pkg/platform/nats does not set nats.NoEcho while the per-session JWT grants
// both pub and sub on ap.session.<ns>.<name>.>, so a wildcard hands each one
// back as a phantom "user replied": await returns instantly, the inbox drain
// finds nothing, and the session burns an LLM round trip instead of parking at
// Idle. Every other IN kind already has its own dedicated subscriber and none
// of them means a new user turn exists.
func inboundWakeSubject(prefix string) string {
	return channelevents.SubjectIn(prefix, channelevents.KindUserMessage)
}

// initNATS connects (returns nil, nil if NATS is not configured), subscribes
// to inboundWakeSubject(prefix) for the channel-attached session, and returns a
// runtime caller can use as the await_user_message wakeup channel.
//
// inboundCh is buffered(1) so a wake-up posted while the agent is mid-turn
// is not lost before the next select.
func initNATS(ctx context.Context, natsURL, prefix string) (*natsRuntime, error) {
	if natsURL == "" {
		return nil, nil
	}
	conn, err := apnats.ConnectFromEnv(natsURL, "runner-"+prefix)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	r := &natsRuntime{conn: conn, inboundCh: make(chan struct{}, 1)}
	sub, err := conn.Subscribe(inboundWakeSubject(prefix), func(_ *nats.Msg) {
		// Drop on full to keep the channel as a bounded wake-up signal.
		select {
		case r.inboundCh <- struct{}{}:
		default:
		}
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("nats subscribe: %w", err)
	}
	r.sub = sub
	return r, nil
}

func (r *natsRuntime) close() {
	if r == nil {
		return
	}
	if r.sub != nil {
		_ = r.sub.Drain()
	}
	if r.conn != nil {
		_ = r.conn.Drain()
	}
}

func natsURLFromEnv() string       { return os.Getenv("NATS_URL") }
func channelAttachedFromEnv() bool { return os.Getenv("CHANNEL_ATTACHED") == "true" }

// natsPublishFunc returns a publish function backed by nc. If nc is nil it
// returns a function that always errors so callers get a clear diagnostic.
func natsPublishFunc(nc *nats.Conn) func(ctx context.Context, subject string, payload []byte) error {
	if nc == nil {
		return func(_ context.Context, _ string, _ []byte) error {
			return fmt.Errorf("NATS not configured")
		}
	}
	return func(_ context.Context, subject string, payload []byte) error {
		return nc.Publish(subject, payload)
	}
}

// subagentSendFunc returns reply_to_subagent's delivery function: it publishes
// one agent_message_send envelope on THIS session's own inbound subject,
// naming the destination child in the payload, and channelsd delivers it
// through the Channel that joins the pair.
//
// It publishes on the sender's own prefix, not the child's, because that is
// the only place a runner's per-session NATS grant authorizes — see
// runnerNATSUserGrant (pkg/controllers/agentsession) and
// channelevents.KindAgentMessageSend. Fire-and-forget, like every other
// runner publish: what the caller learns from a nil return is that the
// envelope reached the bus, and a refusal further along (no Channel joins the
// pair, agentsession#converse denied) surfaces as the tool's own wait timing
// out, plus a channelsd log line naming both ends.
//
// A nil connection returns nil rather than a stub: the meta tool refuses the
// call explicitly when it has no way to send, which is a better message than
// a publish error phrased as a delivery failure.
func subagentSendFunc(rt *natsRuntime, ns, name string, signer *channelevents.EnvelopeSigner) func(ctx context.Context, childNS, childName, text string) error {
	if rt == nil || rt.conn == nil {
		return nil
	}
	return subagentSendPublish(rt.conn.Publish, ns, name, signer)
}

// subagentSendPublish is the publish-closure half of subagentSendFunc,
// factored out so it can be tested against a fake PublishFunc without a real
// NATS connection.
func subagentSendPublish(publish channelevents.PublishFunc, ns, name string, signer *channelevents.EnvelopeSigner) func(ctx context.Context, childNS, childName, text string) error {
	return func(_ context.Context, childNS, childName, text string) error {
		return signer.PublishIn(
			publish, ns, name, channelevents.KindAgentMessageSend,
			channelevents.AgentMessageSendPayload{
				To:   channelevents.SessionRef{Namespace: childNS, Name: childName},
				Text: text,
			},
		)
	}
}

// natsRequestFunc returns a request/reply function backed by nc. If nc
// is nil it returns a function that always errors so callers get a clear
// diagnostic.
func natsRequestFunc(nc *nats.Conn) func(ctx context.Context, subject string, payload []byte) ([]byte, error) {
	if nc == nil {
		return func(context.Context, string, []byte) ([]byte, error) {
			return nil, fmt.Errorf("NATS not configured")
		}
	}
	return func(ctx context.Context, subject string, payload []byte) ([]byte, error) {
		msg, err := nc.RequestWithContext(ctx, subject, payload)
		if err != nil {
			return nil, err
		}
		return msg.Data, nil
	}
}

// toolSessionRegistry routes inbound tool-session input to the live bridge for
// a given ToolCall. Keyed by ToolCall CR name. Safe for concurrent use.
type toolSessionRegistry struct {
	mu    sync.Mutex
	feeds map[string]func([]byte) error
}

func newToolSessionRegistry() *toolSessionRegistry {
	return &toolSessionRegistry{feeds: map[string]func([]byte) error{}}
}

// register installs feed for toolCallRef and returns a cancel func that
// removes it. Safe to call cancel after the registry is GCed.
func (r *toolSessionRegistry) register(ref string, feed func([]byte) error) func() {
	r.mu.Lock()
	r.feeds[ref] = feed
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.feeds, ref)
		r.mu.Unlock()
	}
}

// feed routes data to the registered Feed func for ref. Returns false when
// no bridge is registered (caller logs and drops).
func (r *toolSessionRegistry) feed(ref string, data []byte) bool {
	r.mu.Lock()
	f := r.feeds[ref]
	r.mu.Unlock()
	if f == nil {
		return false
	}
	if err := f(data); err != nil {
		slog.Info("tool_session feed errored", "toolCallRef", ref, "err", err.Error())
	}
	return true
}

// toolSessionInputSubject returns the session-scoped NATS subject for
// inbound KindToolSessionInput messages addressed to the given session.
func toolSessionInputSubject(ns, name string) string {
	return channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindToolSessionInput)
}

// interruptRequestSubject returns the session-scoped NATS subject for inbound
// KindInterruptRequest messages. Unlike approval (runner-initiated), an
// interrupt is channel-initiated: the listener publishes here and the runner
// subscribes directly, with no channelsd hop.
func interruptRequestSubject(ns, name string) string {
	return channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindInterruptRequest)
}

// handleInterruptRequest is the pure, testable core of
// subscribeInterruptRequest: decode, loop.Interrupt, build the applied payload.
// ok=false means data was malformed — the caller logs and publishes nothing.
func handleInterruptRequest(loop *runner.Loop, data []byte) (channelevents.InterruptAppliedPayload, bool) {
	var env channelevents.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return channelevents.InterruptAppliedPayload{}, false
	}
	var pl channelevents.InterruptRequestPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return channelevents.InterruptAppliedPayload{}, false
	}
	// Bounded, not context.Background(): loop.Interrupt itself is fast, but
	// its best-effort per-tool Cancel teardown runs on this NATS subscription
	// goroutine, and a slow Cancel must not stall the subscription forever.
	interruptCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := loop.Interrupt(interruptCtx)
	outcome := "rejected"
	if out.Interrupted {
		outcome = "interrupted"
	}
	return channelevents.InterruptAppliedPayload{
		RequestID:   pl.RequestID,
		Outcome:     outcome,
		Reason:      out.Reason,
		ResponseURL: pl.ResponseURL,
	}, true
}

// subscribeInterruptRequest handles inbound KindInterruptRequest envelopes and
// publishes the resulting KindInterruptApplied outbound so channelsd can render
// the outcome.
//
// Returns when ctx is cancelled. Subscribe/publish errors are logged with
// session context but are not fatal: such a runner merely cannot be interrupted
// mid-turn, and is not otherwise degraded.
func subscribeInterruptRequest(ctx context.Context, rt *natsRuntime, loop *runner.Loop, ns, name string, signer *channelevents.EnvelopeSigner) {
	if rt == nil || rt.conn == nil || loop == nil {
		return
	}
	sub, err := rt.conn.Subscribe(
		interruptRequestSubject(ns, name),
		func(m *nats.Msg) {
			applied, ok := handleInterruptRequest(loop, m.Data)
			if !ok {
				slog.Info("interrupt_request: unmarshal payload failed", "session", ns+"/"+name)
				return
			}
			if perr := signer.PublishOut(
				rt.conn.Publish, ns, name, channelevents.KindInterruptApplied, applied,
			); perr != nil {
				slog.Info("interrupt_applied publish failed",
					"session", ns+"/"+name, "requestID", applied.RequestID, "err", perr.Error())
			}
		},
	)
	if err != nil {
		slog.Info("interrupt_request subscribe failed", "session", ns+"/"+name, "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// appToolCallSubject returns the session-scoped NATS subject for inbound
// KindAppToolCall requests (browser widget → webd → runner). Unlike the
// interrupt subject, the runner answers these with msg.Respond rather than
// publishing an outbound envelope.
func appToolCallSubject(ns, name string) string {
	return channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindAppToolCall)
}

// subscribeAppToolCall answers inbound KindAppToolCall requests (msg.Respond)
// with loop.HandleAppToolCall's readonly-gated, interact-re-authorized
// proxy-exec of an MCP-UI widget's app-tool call.
//
// This is the first of three sibling request/reply responders here
// (app_tool_call, ui_data_binding, ui_action); they differ only in which Loop
// method answers, and share the contract stated below.
//
// ia is the runner's independent interact re-check — the same *spicedb.Client
// the use_token gate uses. A nil ia FAILS CLOSED: the handler denies.
//
// Returns when ctx is cancelled. Subscribe/respond errors are logged with
// session context but are not fatal: such a runner simply cannot serve browser
// calls of this kind, and is not otherwise degraded.
func subscribeAppToolCall(ctx context.Context, rt *natsRuntime, loop *runner.Loop, ia runner.InteractChecker, ns, name string) {
	if rt == nil || rt.conn == nil || loop == nil {
		return
	}
	sub, err := rt.conn.Subscribe(
		appToolCallSubject(ns, name),
		func(m *nats.Msg) {
			resp := loop.HandleAppToolCall(ctx, ia, ns, name, m.Data)
			b, merr := json.Marshal(resp)
			if merr != nil {
				slog.Info("app_tool_call: marshal response failed",
					"session", ns+"/"+name, "err", merr.Error())
				return
			}
			if rerr := m.Respond(b); rerr != nil {
				slog.Info("app_tool_call respond failed",
					"session", ns+"/"+name, "status", resp.Status, "err", rerr.Error())
			}
		},
	)
	if err != nil {
		slog.Info("app_tool_call subscribe failed", "session", ns+"/"+name, "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// uiDataBindingSubject returns the session-scoped NATS subject for inbound
// KindUIDataBinding requests. It stays distinct from app_tool_call despite the
// identical wire shape because the two kinds carry different runner-side
// preconditions — see channelevents.KindUIDataBinding.
func uiDataBindingSubject(ns, name string) string {
	return channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindUIDataBinding)
}

// subscribeUIDataBinding answers inbound KindUIDataBinding requests with
// loop.HandleUIDataBinding's readonly-gated, interact-re-authorized resolution
// of ONE agent-UI data binding whose source is "tool". Same responder contract
// as subscribeAppToolCall, including nil ia failing closed.
func subscribeUIDataBinding(ctx context.Context, rt *natsRuntime, loop *runner.Loop, ia runner.InteractChecker, ns, name string) {
	if rt == nil || rt.conn == nil || loop == nil {
		return
	}
	sub, err := rt.conn.Subscribe(
		uiDataBindingSubject(ns, name),
		func(m *nats.Msg) {
			resp := loop.HandleUIDataBinding(ctx, ia, ns, name, m.Data)
			b, merr := json.Marshal(resp)
			if merr != nil {
				slog.Info("ui_data_binding: marshal response failed",
					"session", ns+"/"+name, "err", merr.Error())
				return
			}
			if rerr := m.Respond(b); rerr != nil {
				slog.Info("ui_data_binding respond failed",
					"session", ns+"/"+name, "status", resp.Status, "err", rerr.Error())
			}
		},
	)
	if err != nil {
		slog.Info("ui_data_binding subscribe failed", "session", ns+"/"+name, "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// uiActionSubject returns the session-scoped NATS subject for inbound
// KindUIAction requests. Distinct from its two siblings because KindUIAction
// carries a third wire shape (channelevents.UIActionRequest) and drives the
// lifecycle recorder neither of them touches.
func uiActionSubject(ns, name string) string {
	return channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindUIAction)
}

// subscribeUIAction answers inbound KindUIAction requests with
// loop.HandleUIAction's viewer-re-authorized invocation of ONE agent-UI action
// binding. Same responder contract as subscribeAppToolCall, including nil ia
// failing closed.
//
// ctx MUST be the long-lived rootCtx: a side-effecting action detaches an
// executeToolContained goroutine that has to outlive this handler's m.Respond
// and die only at pod shutdown.
func subscribeUIAction(ctx context.Context, rt *natsRuntime, loop *runner.Loop, ia runner.InteractChecker, ns, name string) {
	if rt == nil || rt.conn == nil || loop == nil {
		return
	}
	sub, err := rt.conn.Subscribe(
		uiActionSubject(ns, name),
		func(m *nats.Msg) {
			resp := loop.HandleUIAction(ctx, ia, ns, name, m.Data)
			b, merr := json.Marshal(resp)
			if merr != nil {
				slog.Info("ui_action: marshal response failed",
					"session", ns+"/"+name, "err", merr.Error())
				return
			}
			if rerr := m.Respond(b); rerr != nil {
				slog.Info("ui_action respond failed",
					"session", ns+"/"+name, "state", resp.State, "err", rerr.Error())
			}
		},
	)
	if err != nil {
		slog.Info("ui_action subscribe failed", "session", ns+"/"+name, "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// subscribeToolSessionInput routes inbound KindToolSessionInput envelopes to
// the matching bridge. Modeled on subscribeApprovalApplied.
func subscribeToolSessionInput(ctx context.Context, rt *natsRuntime, reg *toolSessionRegistry, ns, name string) {
	if rt == nil || rt.conn == nil || reg == nil {
		return
	}
	sub, err := rt.conn.Subscribe(
		toolSessionInputSubject(ns, name),
		func(m *nats.Msg) {
			var env channelevents.Envelope
			if uerr := json.Unmarshal(m.Data, &env); uerr != nil {
				slog.Info("tool_session_input: unmarshal envelope", "err", uerr.Error())
				return
			}
			var pl channelevents.ToolSessionInputPayload
			if uerr := json.Unmarshal(env.Payload, &pl); uerr != nil {
				slog.Info("tool_session_input: unmarshal payload", "err", uerr.Error())
				return
			}
			if !reg.feed(pl.ToolCallRef, pl.Data) {
				slog.Info("tool_session_input: no live bridge", "toolCallRef", pl.ToolCallRef)
			}
		},
	)
	if err != nil {
		slog.Info("tool_session_input subscribe failed", "err", err.Error())
		return
	}
	<-ctx.Done()
	_ = sub.Drain()
}

// natsConnAdapter adapts a *nats.Conn to the revocation.NATSSubscriber
// interface so the revocation subscriber can work against either a real
// NATS connection in production or a fakeNATS in tests.
type natsConnAdapter struct{ conn *nats.Conn }

func (a *natsConnAdapter) Subscribe(subject string, handler func([]byte)) error {
	_, err := a.conn.Subscribe(subject, func(msg *nats.Msg) { handler(msg.Data) })
	return err
}

// subscribeRevocation wires the oap.revocation subject to the in-flight
// revocation registry, so a published revoke takes effect on the next tool call
// without a pod restart.
//
// onRevoked is called after each scope-matched invalidation with the revoked
// resource's kind and key; the runner emits a Revoked lifecycle event from it so
// the revocation lands in the signed audit log and is re-applied on restart.
//
// Returns an error so a failed subscribe is FAIL-LOUD at startup — a silent skip
// would leave a revoked origin or credential live. A nil rt/rt.conn is a no-op:
// non-channel-attached sessions never receive revoke events.
func subscribeRevocation(ctx context.Context, rt *natsRuntime, reg *revocation.Registry, ns string, onRevoked func(kind, key string)) error {
	if rt == nil || rt.conn == nil {
		return nil
	}
	return subscribeRevocationOnBus(ctx, &natsConnAdapter{conn: rt.conn}, reg, ns, onRevoked)
}

// newRevocationRegistry builds the ONE registry that both the live
// ap.revocation subscriber and claimAndRecover's restart re-application dispatch
// through, so a revocable kind cannot be handled live but dropped on restart.
// Dispatch is by kind, never by matching a literal kind string.
//
// credInvs must be EVERY holder of a resolved credential: the broker's cache and
// the mcpAuthInvalidator over the live MCPTools. Dropping the broker cache alone
// is not enough — MCPTool freezes its (header, value) and never re-reads it.
//
// Wrapping in surfacingInvalidator happens here, at the registry, because both
// dispatch paths go through it: a failed Invalidate then reaches the session's
// participants and not just the log. A nil notifier reports nothing.
func newRevocationRegistry(revokedOrigins *toolorigin.Set, notifier *revocationFailureNotifier, credInvs ...credential.SecretInvalidator) (*revocation.Registry, error) {
	reg := revocation.NewRegistry()
	for _, inv := range []revocation.Invalidator{revokedOrigins, credential.New(credInvs...)} {
		if err := reg.Register(&surfacingInvalidator{inner: inv, report: notifier.report}); err != nil {
			return nil, fmt.Errorf("revocation: register %s invalidator: %w", inv.Kind(), err)
		}
	}
	return reg, nil
}

// subscribeRevocationOnBus is the testable core of subscribeRevocation: it
// accepts an arbitrary NATSSubscriber so tests can inject a fake bus without
// needing a real *nats.Conn.
func subscribeRevocationOnBus(ctx context.Context, bus revocation.NATSSubscriber, reg *revocation.Registry, ns string, onRevoked func(kind, key string)) error {
	if err := revocation.RegisterSubscriber(ctx, bus, reg, ns, onRevoked); err != nil {
		return fmt.Errorf("revocation: subscribe %s: %w", revocation.Subject, err)
	}
	return nil
}

// uiPresenceSubject is the subject webd republishes a liveness heartbeat on
// while a viewer has this session's agent-defined UI open and focused.
func uiPresenceSubject(ns, name string) string {
	return channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindUIPresence)
}

// subscribeUIPresence forwards ui_presence heartbeats into presenceCh, which
// await_user_message uses to restart its idle timer. It never Responds: a
// heartbeat that had to be answered would let a watching browser block on the
// runner, and the next one is strictly fresher than a retry.
//
// The send is non-blocking and dropping is CORRECT, not merely convenient — the
// channel means "at least one heartbeat since the last read", so a second adds
// nothing, while blocking would park a NATS callback on the loop's scheduling.
//
// The payload is deliberately NOT decoded. Presence is a liveness hint and
// never an authorization input; authorization is re-asked per binding call.
//
// Returns when ctx is cancelled. A subscribe failure is logged, not fatal: the
// runner still serves every binding, it just idles out on its base TTL.
func subscribeUIPresence(ctx context.Context, rt *natsRuntime, presenceCh chan<- struct{}, ns, name string) {
	if rt == nil || rt.conn == nil || presenceCh == nil {
		return
	}
	sub, err := rt.conn.Subscribe(
		uiPresenceSubject(ns, name),
		func(*nats.Msg) {
			select {
			case presenceCh <- struct{}{}:
			default:
			}
		},
	)
	if err != nil {
		slog.Info("ui_presence subscribe failed; this runner will idle out on its base TTL even while a viewer is watching",
			"session", ns+"/"+name, "err", err.Error())
		return
	}
	<-ctx.Done()
	if derr := sub.Drain(); derr != nil {
		slog.Info("ui_presence drain failed", "session", ns+"/"+name, "err", derr.Error())
	}
}

// runServeOnly parks a serve-only runner: it holds the process open so the
// already-subscribed browser responders keep answering, and exits when no viewer
// has been seen for idleTTL.
//
// It runs NO agent loop, and that is the point. Loop.Run is the sole
// terminal-status writer and sole emitter of SigSessionStarted, so returning
// here instead cannot move the session's phase, write a terminal status, or cost
// a turn — the session stays exactly as it was found.
//
// The wait is presence-driven because a dashboard's data bindings involve no
// conversation: "is anyone still looking" is the right question, not "has anyone
// spoken". The initial deadline is granted WITHOUT a heartbeat — the browser
// cannot heartbeat until its socket is up, so requiring one would race the very
// request that caused this pod to spawn.
func runServeOnly(ctx context.Context, presenceCh <-chan struct{}, idleTTL time.Duration, ns, name string) error {
	session := ns + "/" + name
	if idleTTL <= 0 {
		slog.Info("serve-only: idle ttl is disabled, so there is no window in which to serve; exiting immediately",
			"session", session)
		return nil
	}
	slog.Info("serve-only: serving agent-UI requests without an agent loop; no turn will be taken and the session's phase is untouched",
		"session", session, "idleTTL", idleTTL.String())

	timer := time.NewTimer(idleTTL)
	defer timer.Stop()
	served := 0
	for {
		select {
		case <-presenceCh:
			served++
			timer.Stop()
			timer = time.NewTimer(idleTTL)
		case <-timer.C:
			slog.Info("serve-only: no viewer seen within the idle window; exiting cleanly",
				"session", session, "heartbeats", served)
			return nil
		case <-ctx.Done():
			slog.Info("serve-only: context canceled (SIGTERM); exiting cleanly",
				"session", session, "heartbeats", served)
			return nil
		}
	}
}
