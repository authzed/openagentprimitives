package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/extract"
	"github.com/authzed/openagentprimitives/pkg/authz/slotspec"
	"github.com/authzed/openagentprimitives/pkg/memory"
	asc "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
	exs "github.com/authzed/openagentprimitives/pkg/memory/kinds/extraction_state"
	turnkind "github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// Worker subscribes to KindUserMessage envelopes (one subscription
// session-wide) and routes per-session into a per-session goroutine that
// runs extraction serially.
type Worker struct {
	deps     WorkerDeps
	mu       sync.Mutex
	sessions map[string]chan struct{} // session-key → wake channel
}

// WorkerDeps holds the external dependencies injected into Worker.
type WorkerDeps struct {
	Memory      memory.Memory
	Extractor   extract.Provider
	IdleTimeout time.Duration
	// ExtractTimeout caps the LLM call. Zero → 10s.
	ExtractTimeout time.Duration
}

// NewWorker constructs a Worker with defaults applied.
func NewWorker(d WorkerDeps) *Worker {
	if d.IdleTimeout == 0 {
		d.IdleTimeout = 10 * time.Minute
	}
	if d.ExtractTimeout == 0 {
		d.ExtractTimeout = 10 * time.Second
	}
	return &Worker{deps: d, sessions: map[string]chan struct{}{}}
}

// Handle is the entry point a NATS subscription calls when a user_message
// arrives for the given session.
func (w *Worker) Handle(ctx context.Context, scope memory.Scope) error {
	key := scope.ID
	// The lock is held across the send, not just the map lookup: releaseSession
	// retires a channel under this same lock, so a send made after unlocking
	// could land in a buffer whose only reader is already returning — a lost
	// wake-up that Handle would still report as delivered. The send never
	// blocks (buffered channel plus the default arm below), so holding the
	// mutex across it costs nothing.
	w.mu.Lock()
	defer w.mu.Unlock()
	ch, ok := w.sessions[key]
	if !ok {
		ch = make(chan struct{}, 16) // small buffer; queue group serializes inbox-turn order
		w.sessions[key] = ch
		go w.runSession(scope, ch)
	}
	select {
	case ch <- struct{}{}:
		return nil
	default:
		// Buffer full — extractor is busy AND backlog already queued.
		// Drop with a log; missed wake-up means autofill skipped for that
		// turn, which is the documented degradation.
		slog.Info("authzd: session wake-channel full; dropping signal", "session", key)
		return nil
	}
}

func (w *Worker) runSession(scope memory.Scope, ch <-chan struct{}) {
	idle := time.NewTimer(w.deps.IdleTimeout)
	defer idle.Stop()
	for {
		select {
		case <-ch:
			if !idle.Stop() {
				<-idle.C
			}
			w.processBacklog(context.Background(), scope)
			idle.Reset(w.deps.IdleTimeout)
		case <-idle.C:
			if w.releaseSession(scope.ID, ch) {
				slog.Info("authzd: session goroutine idle exit", "session", scope.ID)
				return
			}
			idle.Reset(w.deps.IdleTimeout)
		}
	}
}

// releaseSession retires the per-session goroutine's registration when the idle
// timer fires: it removes the wake channel from the map (so a later Handle
// spawns a fresh goroutine) and reports whether the goroutine may exit.
//
// Handle buffers its wake-up while holding w.mu, so taking that same lock here
// makes "registered in the map" and "has a live reader" one atomic fact. A
// signal that arrived between the timer firing and this call is still in the
// buffer: keep the registration and let the caller keep looping, or the
// wake-up is stranded in a channel nobody will ever read again and the
// session's extraction stalls until some later message respawns the goroutine.
func (w *Worker) releaseSession(key string, ch <-chan struct{}) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(ch) > 0 {
		return false
	}
	delete(w.sessions, key)
	return true
}

// processBacklog reads every inbox-role turn lacking an extraction_state
// entry and runs the prefilter → extract → memory.Put pipeline for each,
// in index order. Per-session goroutine guarantees serial execution.
//
// This worker handles ONLY entity extraction for autofill. Cold-start scope is
// driven by the runner at the initial-prompt site
// (pkg/agent/runner (*Loop).coldStartSessionStartExecutor, dispatched from
// Loop.Run), because a new channel session's first message is the
// AgentSession's spec.Prompt — placed as turn 0 — not an inbox turn, so no
// inbox-turn trigger here would ever fire for it.
func (w *Worker) processBacklog(ctx context.Context, scope memory.Scope) {
	cfg, ok, err := asc.Get(ctx, w.deps.Memory, scope)
	if err != nil || !ok {
		slog.Info("authzd: no session config; skipping", "session", scope.ID, "err", err)
		return
	}
	for _, t := range w.findUnprocessedInbox(ctx, scope) {
		w.processOne(ctx, scope, cfg, t)
	}
}

// findUnprocessedInbox returns inbox-role turns lacking an extraction_state
// entry, sorted by Index ascending.
func (w *Worker) findUnprocessedInbox(ctx context.Context, scope memory.Scope) []memory.Turn {
	all, err := turnkind.ReadAll(ctx, w.deps.Memory, scope)
	if err != nil {
		// A memory read failure here silently skips the whole session's
		// entity extraction; log it (mirrors processBacklog's logged skip)
		// so an operator can locate the failing read.
		slog.Info("authzd: read turns failed; skipping extraction for session", "session", scope.ID, "err", err)
		return nil
	}
	var out []memory.Turn
	for _, t := range all {
		if t.Role != "inbox" {
			continue
		}
		_, has, lookupErr := exs.ForTurn(ctx, w.deps.Memory, scope, t.Index)
		if lookupErr != nil {
			// On an extraction_state read error has=false would re-queue the
			// turn for re-extraction; prefer skipping (treat as processed) and
			// log so the diagnostic isn't lost.
			slog.Info("authzd: extraction_state lookup failed; skipping turn", "session", scope.ID, "turn", t.Index, "err", lookupErr)
			continue
		}
		if has {
			continue
		}
		out = append(out, t)
	}
	return out
}

// processOne runs entity extraction for a single inbox turn through the generic
// pipeline executor (the EntityBind hook): prefilter → extractor LLM →
// extracted_entity / extraction_state. The per-turn work is delegated to
// runExtractionViaExecutor; the worker keeps the goroutine/idle/backlog/cursor
// machinery and the authz_session_config read.
func (w *Worker) processOne(ctx context.Context, scope memory.Scope, cfg asc.Content, t memory.Turn) {
	if err := w.runExtractionViaExecutor(ctx, scope, cfg, t); err != nil {
		// EntityBind is advisory (never gates), so the executor only returns an
		// error on a host-primitive failure — which EntityBind's path never
		// triggers. Log it (no silent errors) for diagnosability.
		slog.Info("authzd: extraction executor error", "session", scope.ID, "turn", t.Index, "err", err)
	}
}

// firstTextBlock returns the text of the first "text" content block in the
// turn, or empty string if none.
func firstTextBlock(t memory.Turn) string {
	for _, b := range t.Content {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}

// slotsFillableByQuery keeps only the slots whose fillFrom admits the
// query/extract source. Filtering on the v1alpha1 type (rather than after the
// adapter) is what lets the extractor PROMPT be narrowed too — it is built from
// the v1alpha1 slots, not from the pkg/authz specs.
func slotsFillableByQuery(in []spiceboxv1alpha1.BoundEntityType) []spiceboxv1alpha1.BoundEntityType {
	out := make([]spiceboxv1alpha1.BoundEntityType, 0, len(in))
	for _, e := range in {
		if authz.AllowsExtractedBinding(e.FillFrom) {
			out = append(out, e)
		}
	}
	return out
}

// The conversion is pkg/authz/slotspec's, the only place a BoundEntitySpec is
// constructed. This site used to write its own literal and, being the one that
// feeds the extractor prefilter rather than a binder, was the easiest of the
// three to leave a field off — which for `requires[]` means a slot with no
// gate.
//
// nil transforms: authzd holds the session config's spec-side slot list, not
// the AgentClass status that resolves value-transform chains. Passing nil
// preserves this call site's existing behavior exactly — these specs decide
// which slot types the extractor should look for and never reach bindSlots.
func toBoundEntitySpecs(in []spiceboxv1alpha1.BoundEntityType) ([]authz.BoundEntitySpec, error) {
	return slotspec.FromSlots(in, nil)
}
