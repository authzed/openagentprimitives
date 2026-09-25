// Package approval implements a per-runner-process orchestrator for
// tool-call approval pauses. The dispatch goroutine that detects "needs
// approval" calls Await, which (optionally) publishes the approval
// envelope and blocks on a per-request channel. The runner's NATS
// subscriber calls DeliverDecision when a KindToolApprovalApplied
// envelope arrives; the matching channel receives and Await returns.
package approval

import (
	"context"
	"fmt"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Decision is the outcome of an approval request as delivered via the
// approval envelope back-channel.
type Decision struct {
	Approved bool

	// ApproverID is the approver's RAW external id — a Slack user id, an email,
	// whatever the channel calls them. It is for DISPLAY and audit text. It is
	// not a SpiceDB subject and is never canonical, so nothing may hand it to
	// an authorization call.
	ApproverID string

	// Approver is that same person as a structured principal, and it is what
	// any authorization question about them must be asked with.
	//
	// Both, because they answer different questions and collapsing them is what
	// broke: a delegation lookup took ApproverID, wrapped it in
	// identity.CanonicalUserID — a TYPE, so a conversion rather than an
	// encoding — and asked SpiceDB about `user:admin@ap.local`. SpiceDB refused
	// the object id, the lookup errored, the approval was discarded, and the
	// person who had just clicked Approve was told nothing and asked again.
	//
	// Zero value is a principal that cannot canonicalize, which fails closed.
	Approver identity.Principal

	Reason string
	// Action is a free-form, request-kind-specific refinement of the
	// approval outcome. For cold-start scope approvals it carries the
	// chosen button ("approve_cleaned" / "approve_original" /
	// "run_without_scope" / "deny"); the decider maps it to the matching
	// ColdStart action. Empty for mid-session approvals and tool-call
	// approvals, where Approved alone determines the outcome.
	Action string
}

// Request describes a single approval pause. RequestID is the unique
// correlation ID expected on the inbound KindToolApprovalApplied
// envelope. SessionRef is informational and used for PendingForSession
// queries (e.g., session-shutdown cleanup). OnPublish, when non-nil, is
// invoked synchronously after the request is registered but before
// blocking on the decision; failures abort the await before the block.
type Request struct {
	RequestID  string
	SessionRef string
	OnPublish  func(ctx context.Context) error
}

// Orchestrator tracks in-flight approval requests inside a single runner
// process. It is safe for concurrent use.
type Orchestrator struct {
	mu      sync.Mutex
	pending map[string]chan Decision
	bySess  map[string]map[string]struct{}
}

// New returns an empty Orchestrator.
func New() *Orchestrator {
	return &Orchestrator{
		pending: map[string]chan Decision{},
		bySess:  map[string]map[string]struct{}{},
	}
}

// Await registers an approval request, optionally publishes it (via
// r.OnPublish), and blocks until DeliverDecision is called with the
// matching RequestID or ctx is cancelled. On ctx cancel, Await returns
// the ctx error and a Decision with Reason="timeout"; the request is
// also forgotten so DeliverDecision becomes a no-op.
func (o *Orchestrator) Await(ctx context.Context, r Request) (Decision, error) {
	if r.RequestID == "" {
		return Decision{}, fmt.Errorf("approval: empty RequestID")
	}
	ch := make(chan Decision, 1)
	o.mu.Lock()
	o.pending[r.RequestID] = ch
	if r.SessionRef != "" {
		if o.bySess[r.SessionRef] == nil {
			o.bySess[r.SessionRef] = map[string]struct{}{}
		}
		o.bySess[r.SessionRef][r.RequestID] = struct{}{}
	}
	o.mu.Unlock()

	defer o.forget(r.RequestID, r.SessionRef)

	if r.OnPublish != nil {
		if err := r.OnPublish(ctx); err != nil {
			return Decision{}, fmt.Errorf("publish approval request: %w", err)
		}
	}

	select {
	case d := <-ch:
		return d, nil
	case <-ctx.Done():
		return Decision{Reason: "timeout"}, ctx.Err()
	}
}

// DeliverDecision routes d to the Awaiter blocked on requestID, if any.
// Unknown / already-resolved request IDs are silently dropped — this is
// expected for late-arriving decisions on timed-out awaits.
func (o *Orchestrator) DeliverDecision(requestID string, d Decision) {
	o.mu.Lock()
	ch, ok := o.pending[requestID]
	o.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- d:
	default:
	}
}

// PendingForSession returns the number of currently-awaiting requests
// associated with sessionRef. Used by the runner to gate session
// shutdown on outstanding approvals.
func (o *Orchestrator) PendingForSession(sessionRef string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.bySess[sessionRef])
}

// PendingRequestIDsForSession returns the IDs of currently-awaiting
// requests for sessionRef. Order is unspecified.
func (o *Orchestrator) PendingRequestIDsForSession(sessionRef string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.bySess[sessionRef]
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	return out
}

func (o *Orchestrator) forget(requestID, sessionRef string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.pending, requestID)
	if sessionRef != "" {
		if set, ok := o.bySess[sessionRef]; ok {
			delete(set, requestID)
			if len(set) == 0 {
				delete(o.bySess, sessionRef)
			}
		}
	}
}
