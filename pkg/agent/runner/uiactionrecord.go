package runner

import (
	"context"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
)

// uiActionRecorder is one agent-UI action call's lifecycle. It writes the
// durable ui_action memory record and publishes the ui_action_update push,
// in that order, for every transition.
//
// Order is load-bearing and not a style choice: memory is the SOURCE OF
// TRUTH, the envelope is a latency optimization. Publishing first would let a
// browser observe a state that a subsequent failed write means no
// reconnecting browser can ever observe again — live and reconnect would
// diverge, which is precisely what the design spec's "memory stays the single
// source of truth" rule exists to prevent.
//
// One per call, owned by the goroutine that created it until the detached
// exec takes it over; see settle's context note.
type uiActionRecorder struct {
	l        *Loop
	ns, name string

	requestID string
	action    string
	requester string // bare canonical, via uiaction.RequesterKey

	// timedOut records whether THIS call's approval lapsed rather than being
	// decided. Written by onApproval and read by settle, both of which run on
	// the same goroutine (executeToolContained invokes the observer
	// synchronously — see containParams.approvalEvent), so no mutex is
	// needed. If that ever changes this needs to become an atomic.Bool.
	timedOut bool
}

// newUIActionRecorder builds the per-call lifecycle recorder for one
// agent-UI action invocation. requester must already be the bare canonical
// form (uiaction.RequesterKey) — callers derive it once, at the browser
// boundary, so every write and push in this call's lifecycle agrees.
func newUIActionRecorder(l *Loop, ns, name, requestID, action, requester string) *uiActionRecorder {
	return &uiActionRecorder{
		l:         l,
		ns:        ns,
		name:      name,
		requestID: requestID,
		action:    action,
		requester: requester,
	}
}

// transition writes one state and pushes it. r may be nil (the two other
// appToolSurface callers never build a recorder) — every method on this type
// is a nil-receiver no-op, so call sites never need their own nil check.
func (r *uiActionRecorder) transition(ctx context.Context, state uiaction.State, message string, addressedToViewer bool) {
	if r == nil {
		return
	}
	content := uiaction.Content{
		RequestID:                 r.requestID,
		Action:                    r.action,
		State:                     state,
		Message:                   message,
		UpdatedAt:                 time.Now().UTC(),
		Requester:                 r.requester,
		ApprovalAddressedToViewer: addressedToViewer,
	}

	if r.l == nil || r.l.Mem == nil {
		slog.Default().Info("ui_action lifecycle write skipped: no memory wired",
			"session", r.ns+"/"+r.name, "action", r.action, "requestID", r.requestID, "state", state)
		return
	}
	scope := memory.Scope{Kind: "session", ID: r.ns + "/" + r.name}
	if err := uiaction.Record(ctx, r.l.Mem, scope, content); err != nil {
		// The memory write is the SOURCE OF TRUTH (see the type doc comment): a
		// failed write must never be followed by a push, or a reconnecting
		// browser and a live one would observe different histories. Log and
		// return — this runs on a detached goroutine with nobody above it to
		// hand the error to (no-silent-errors is satisfied by the log).
		slog.Default().Info("ui_action lifecycle write failed",
			"session", r.ns+"/"+r.name, "action", r.action, "requestID", r.requestID,
			"state", state, "err", err.Error())
		return
	}

	if r.l.UIPublish == nil {
		return
	}
	payload := channelevents.UIActionUpdatePayload{
		RequestID:                 r.requestID,
		Action:                    r.action,
		Requester:                 r.requester,
		State:                     string(state),
		Message:                   message,
		ApprovalAddressedToViewer: addressedToViewer,
		UpdatedAt:                 content.UpdatedAt,
	}
	env, err := channelevents.BuildEnvelope(r.ns, r.name, channelevents.KindUIActionUpdate, payload)
	if err != nil {
		slog.Default().Info("ui_action lifecycle push build failed",
			"session", r.ns+"/"+r.name, "action", r.action, "requestID", r.requestID,
			"state", state, "err", err.Error())
		return
	}
	if err := r.l.UIPublish(ctx, r.ns, r.name, env); err != nil {
		slog.Default().Info("ui_action lifecycle push failed",
			"session", r.ns+"/"+r.name, "action", r.action, "requestID", r.requestID,
			"state", state, "err", err.Error())
	}
}

// onApproval is the containParams.approvalEvent this recorder installs. It
// turns the runner's two approval observations into the two lifecycle states
// nothing else can see:
//
//	Asked    -> awaiting_approval, carrying AddressedToViewer (the chrome
//	            auto-reveal signal, and the ONLY route it has to a browser)
//	Resolved -> running, when approved; a timeout is remembered rather than
//	            written, because the terminal write is what turns it into
//	            `expired` (a lapsed deadline resolves to a Deny verdict, so the
//	            phase alone cannot tell the two apart).
//
// Invoked SYNCHRONOUSLY on the goroutine running executeToolContained (see
// containParams.approvalEvent's doc comment), with no ctx of its own to
// inherit: neither PublishApproval nor the point where AwaitDecision resolves
// threads one to this callback. context.Background() is the same choice
// host_approval.go's own memapproval.RecordRequest and RecordOutcome calls
// make, for the identical reason.
func (r *uiActionRecorder) onApproval(ev approvalObservation) {
	if r == nil {
		return
	}
	ctx := context.Background()
	switch {
	case ev.Asked:
		r.transition(ctx, uiaction.StateAwaitingApproval,
			uiaction.DisplayCopy(uiaction.StateAwaitingApproval, ev.AddressedToViewer), ev.AddressedToViewer)
	case ev.Resolved && ev.TimedOut:
		r.timedOut = true
	case ev.Resolved && ev.Approved:
		r.transition(ctx, uiaction.StateRunning, uiaction.DisplayCopy(uiaction.StateRunning, false), false)
		// A plain denial (Resolved && !Approved && !TimedOut) writes no
		// intermediate state: settle's terminal "denied" state, derived from
		// the pipeline's Deny outcome via uiaction.StateFor, is the whole story.
	}
}

// settle writes the terminal state derived from a synchronous outcome.
//
// It takes its OWN context, derived with context.WithoutCancel, because the
// most important call to it is made from the detached goroutine AFTER that
// goroutine's context may already have expired — which is exactly the
// `expired` case. Inheriting the cancelled context would mean the one state
// that only this write can record is the one state it can never write, and
// the control would stay disabled forever with nothing in the logs. The
// approval host makes the same choice for the same reason (memapproval.
// RecordRequest is called with a fresh context).
func (r *uiActionRecorder) settle(ctx context.Context, resp channelevents.AppToolCallResponse) {
	if r == nil {
		return
	}
	state := uiaction.StateFor(resp.Status, resp.IsError, r.timedOut)
	// ViewerMessage-or-fallback mirrors HandleUIAction's own synchronous
	// response construction: resp.Message is the containment pipeline's
	// diagnostic text (permission names, resource IDs) and must never reach a
	// browser-facing record; ViewerMessage is the one field authored for a
	// human, and DisplayCopy is this surface's generic fallback when even that
	// is unset (e.g. a plain successful result carries no message at all).
	msg := resp.ViewerMessage
	if msg == "" {
		msg = uiaction.DisplayCopy(state, false)
	}
	r.transition(context.WithoutCancel(ctx), state, msg, false)
}
