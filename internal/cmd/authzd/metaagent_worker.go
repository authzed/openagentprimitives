package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// metaagentRequest is the internal message routed to per-session goroutines.
type metaagentRequest struct {
	scopeRef  memory.Scope
	requester string
	text      string
	// ambient marks the third trigger: classified because the session runs
	// with metaagent.trigger shadow or inline, not because anyone addressed
	// the metaagent. shadow means run the whole path and apply NOTHING.
	ambient bool
	shadow  bool
	// coldStart routes the request to the cold-start branch of the staged
	// lifecycle (new-session first turn) instead of the mid-session one.
	//
	// There is deliberately NO autoApply field: waiving the human approval gate
	// is resolved from the session's authz_session_config snapshot inside
	// runMetaagentLifecycle (see cold_start_policy.go), never carried on the
	// request that wants the gate waived.
	coldStart bool
	// envelope is the static AgentClass envelope (bound entities + tools) the
	// runner assembled and embedded in the metaagent_request payload. It lets
	// ClassifySkipped keep in-envelope scope changes (e.g. a hard-deny
	// narrowing) instead of dropping everything as out-of-envelope.
	envelope scope.AgentClassEnvelope
	// approvalTimeout is the consolidated authz.approvalTimeout from the runner's
	// AgentClass (default 10m). Threaded into the cold-start ColdStartScopeAuthzdDeps
	// so AwaitDecision uses the class's configured value, not the executor's hardcoded
	// default. Zero means "not specified" — the hook leaves ApprovalAsk.Timeout unset
	// and the executor falls back to its 10m default.
	approvalTimeout time.Duration
}

// MetaagentWorker subscribes to KindMetaagentRequest envelopes (one
// subscription session-wide) and routes each into a per-session goroutine that
// runs the metaagent lifecycle serially.
type MetaagentWorker struct {
	mg           *Metaagent
	approvalOrch *approval.Orchestrator
	natsConn     *nats.Conn
	coldStart    *ColdStartHandler

	// manageScopeChecker gates mid-session @metaagent scope changes on the
	// session owner (agentsession#manage_scope = owner). Declared as the
	// interface (AGENTS.md nil-interface rule): internal/cmd/authzd/main.go assigns a real
	// *spicedb.Client-backed checker when SPICEDB_ENDPOINT is set, else leaves it
	// a genuine nil interface (the MetaagentReceived hook then fails closed for
	// mid_session; cold_start skips the gate). authzd REQUIRES SpiceDB at startup,
	// so in production this is always wired.
	manageScopeChecker authz.ManageScopeChecker

	mu       sync.Mutex
	sessions map[string]chan metaagentRequest
}

// NewMetaagentWorker constructs a MetaagentWorker. coldStart handles
// new-session first-turn (cold-start) requests; it may be nil when no LLM
// provider is configured, in which case cold-start requests fall back to a
// logged "unavailable" with no scope applied.
func NewMetaagentWorker(mg *Metaagent, orch *approval.Orchestrator, nc *nats.Conn, coldStart *ColdStartHandler) *MetaagentWorker {
	return &MetaagentWorker{
		mg:           mg,
		approvalOrch: orch,
		natsConn:     nc,
		coldStart:    coldStart,
		sessions:     map[string]chan metaagentRequest{},
	}
}

// SetManageScopeChecker injects the SpiceDB-backed manage_scope checker the
// mid-session MetaagentReceived gate uses. Pass a genuine nil interface (NOT a
// typed-nil pointer) to leave it unwired (mid_session then fails closed).
func (w *MetaagentWorker) SetManageScopeChecker(c authz.ManageScopeChecker) {
	w.manageScopeChecker = c
}

// HandleInput is one decoded metaagent_request.
//
// A struct rather than a positional list, and the reason is the adjacent
// booleans: ColdStart, Ambient and Shadow all mean different things, all
// compile in any order, and transposing two of them would silently route a
// shadow request as one that applies. Several positional parameters is where
// that stops being a hypothetical.
//
// Deliberately NOT AutoApply: waiving the human approval gate is resolved
// from the session's authz_session_config snapshot inside
// runMetaagentLifecycle (see cold_start_policy.go), never carried on the
// request that wants the gate waived.
type HandleInput struct {
	Scope     memory.Scope
	Requester string
	Text      string

	// ColdStart routes to the session-start path.
	ColdStart bool

	// Ambient marks the THIRD trigger: this turn was not addressed to the
	// metaagent, it was classified because the session runs with
	// metaagent.trigger shadow or inline.
	Ambient bool
	// Shadow means classify fully and APPLY NOTHING. Only meaningful with
	// Ambient — an explicit mention always applies, whatever the trigger says.
	Shadow bool

	Envelope        scope.AgentClassEnvelope
	ApprovalTimeout time.Duration
}

// Handle is the NATS subscription callback entry. Looks up or spawns
// the per-session goroutine; pushes the request onto its channel.
func (w *MetaagentWorker) Handle(ctx context.Context, in HandleInput) error {
	key := in.Scope.ID
	w.mu.Lock()
	ch, ok := w.sessions[key]
	if !ok {
		ch = make(chan metaagentRequest, 16)
		w.sessions[key] = ch
		go w.runSession(ctx, key, ch)
	}
	w.mu.Unlock()
	// NON-BLOCKING, and that is the whole point.
	//
	// This runs on the NATS subscription callback, and nats.go dispatches one
	// subscription's callbacks serially — so parking here stops delivery for
	// EVERY session, not just this one. The per-session goroutine that drains
	// this channel can sit inside a 24-hour approval await, so "full" is a
	// state that lasts as long as a human takes to click. One session with an
	// unclicked card plus a handful of follow-ups therefore wedged metaagent
	// processing cluster-wide, and every scope-enabled session then halted
	// fail-closed at cold start waiting for a task that would never arrive.
	//
	// It needed no attacker: a class in shadow mode posts real cards for
	// ordinary owner messages, so a chatty owner got there by accident.
	//
	// Dropping loses one request, which is recoverable — the requester can ask
	// again — and is what the sibling Worker.Handle already does.
	select {
	case ch <- metaagentRequest{
		scopeRef: in.Scope, requester: in.Requester, text: in.Text,
		coldStart: in.ColdStart,
		ambient:   in.Ambient, shadow: in.Shadow,
		envelope: in.Envelope, approvalTimeout: in.ApprovalTimeout,
	}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		slog.Info("authzd: metaagent session queue full; dropping request",
			"session", key, "requester", in.Requester, "coldStart", in.ColdStart)
		return nil
	}
}

func (w *MetaagentWorker) runSession(ctx context.Context, key string, ch chan metaagentRequest) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("metaagent worker panic; goroutine exits and will restart on next message",
				"session", key, "panic", r)
		}
		w.mu.Lock()
		delete(w.sessions, key)
		w.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-ch:
			if !ok {
				return
			}
			w.handleOne(ctx, req)
		}
	}
}

func (w *MetaagentWorker) handleOne(ctx context.Context, req metaagentRequest) {
	// Cold-start and mid-session @metaagent both run the SAME staged
	// control-plane lifecycle (Received → Extract → Decide → Apply); the Kind
	// (cold_start | mid_session) is derived from req.coldStart inside
	// runMetaagentLifecycle and branches the four hooks.
	if err := w.runMetaagentLifecycle(ctx, req); err != nil {
		slog.Error("metaagent: lifecycle error",
			"session", req.scopeRef.ID, "requester", req.requester, "coldStart", req.coldStart, "err", err)
	}
}

func sessRefString(s memory.Scope) string { return s.ID }

// metaagentScopeSession splits a session memory.Scope.ID ("ns/name") into the
// two SEPARATE NATS tokens every ap.session subject is built from. Collapsing
// them into one token ("ap.session.<ns/name>.out.…") produces a subject that no
// "ap.session.*.*.out.<leaf>" subscription matches, so the publish succeeds and
// nothing is ever delivered — which is why this refuses an empty half rather
// than emitting a subject with an empty token.
func metaagentScopeSession(scopeID string) (ns, name string, err error) {
	ns, name, ok := strings.Cut(scopeID, "/")
	if !ok || ns == "" || name == "" {
		return "", "", fmt.Errorf("malformed session scope ID %q", scopeID)
	}
	return ns, name, nil
}
