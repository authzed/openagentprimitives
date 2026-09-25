package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const defaultApprovalTimeout = 10 * time.Minute

// Executor runs the hooks for a lifecycle point and applies their decisions.
// It owns ALL orchestration: ordering, short-circuit, approval await+timeout,
// notice/status delivery, audit, and fail-closed-on-panic.
type Executor struct {
	reg *Registry

	// inflight coalesces concurrent asks that name the same decision. Keyed by
	// ApprovalAsk.CoalesceKey; an empty key never enters the map, so an ask that
	// does not opt in is never merged.
	mu       sync.Mutex
	inflight map[string]*pendingDecision
}

// pendingDecision is one published-and-awaited approval that other callers may
// wait on. done is closed when the outcome is set.
type pendingDecision struct {
	done     chan struct{}
	approved bool
	by       string
	timedOut bool
	err      error
}

func NewExecutor(reg *Registry) *Executor {
	return &Executor{reg: reg, inflight: map[string]*pendingDecision{}}
}

// resolveApproval publishes and awaits an ask, coalescing concurrent callers
// that name the same decision.
//
// The FIRST caller for a key owns the publish and the await; the rest block on
// its result and receive the same verdict. The owner clears the key on
// completion, so a LATER, separate call for the same phase asks again — this
// deduplicates a burst, it does not cache a decision.
func (e *Executor) resolveApproval(ctx context.Context, host Host, ask ApprovalAsk, timeout time.Duration) (approved bool, by string, timedOut bool, err error) {
	if ask.CoalesceKey == "" {
		reqID, perr := host.PublishApproval(ctx, ask)
		if perr != nil {
			return false, "", false, perr
		}
		return host.AwaitDecision(ctx, reqID, timeout)
	}

	e.mu.Lock()
	if p, ok := e.inflight[ask.CoalesceKey]; ok {
		e.mu.Unlock()
		select {
		case <-p.done:
			return p.approved, p.by, p.timedOut, p.err
		case <-ctx.Done():
			// This caller gave up; the owner still owns the decision.
			return false, "", false, ctx.Err()
		}
	}
	p := &pendingDecision{done: make(chan struct{})}
	e.inflight[ask.CoalesceKey] = p
	e.mu.Unlock()

	defer func() {
		p.approved, p.by, p.timedOut, p.err = approved, by, timedOut, err
		close(p.done)
		e.mu.Lock()
		delete(e.inflight, ask.CoalesceKey)
		e.mu.Unlock()
	}()

	reqID, perr := host.PublishApproval(ctx, ask)
	if perr != nil {
		return false, "", false, perr
	}
	return host.AwaitDecision(ctx, reqID, timeout)
}

// Run executes the hooks registered at p, in order, against in, applying their
// effects via host. Returns the final Outcome. The error return is reserved for
// host-primitive failures the executor cannot itself turn into a verdict; hook
// failures become Halt (see Task 6).
func (e *Executor) Run(ctx context.Context, p Point, in Input, host Host) (Outcome, error) {
	in.Point = p
	var notices []Notice
	var audit []AuditRecord

	// def is set from the deciding hook's Decision just before finish is
	// called, rather than threaded as a parameter: every finish call site
	// except the deny/halt arms is reporting an executor-level failure
	// (approval publish/await), which is never a broken definition, so nil is
	// right for them and only the two arms carrying a hook's own verdict set
	// it.
	var def error
	finish := func(v Verdict, reason, fired string) (Outcome, error) {
		// Effect delivery is best-effort: a Notify/SetStatus/Audit failure must not
		// abort the gate decision. The generic executor has no logger; Hosts log
		// their own delivery failures (see Host contract).
		for _, n := range notices {
			_ = host.Notify(ctx, n)
		}
		if len(audit) > 0 {
			_ = host.Audit(ctx, audit)
		}
		return Outcome{Verdict: v, Reason: reason, FiredHook: fired, Definition: def}, nil
	}

	for _, h := range e.reg.Hooks(p) {
		dec := safeEval(ctx, h, in)
		notices = append(notices, dec.Notices...)
		audit = append(audit, dec.Audit...)
		// Effect delivery is best-effort: a Notify/SetStatus/Audit failure must not
		// abort the gate decision. The generic executor has no logger; Hosts log
		// their own delivery failures (see Host contract).
		if dec.Status != nil {
			_ = host.SetStatus(ctx, *dec.Status)
		}
		if dec.Approval != nil {
			timeout := dec.Approval.Timeout
			if timeout <= 0 {
				timeout = defaultApprovalTimeout
			}
			approved, by, timedOut, aerr := e.resolveApproval(ctx, host, *dec.Approval, timeout)
			if aerr != nil && !timedOut {
				// A publish failure arrives here as an error too; both are host
				// primitive failures and fail closed the same way.
				_ = host.Halt(ctx, "approval failed: "+aerr.Error())
				return finish(Halt, "approval failed", h.Name())
			}
			switch {
			case timedOut:
				// A timeout is a verdict, not an error. This is the SOLE site that
				// turns a lapsed deadline into a verdict; the policy stamped on the
				// ask (sourced from the lifecycle decisionParams table) decides.
				// TimeoutHalt fails closed; TimeoutDeny (default) is a sticky-deny
				// that lets the turn continue.
				if dec.Approval.OnTimeout == TimeoutHalt {
					_ = host.Halt(ctx, "approval timed out (fail-closed)")
					return finish(Halt, "approval timed out", h.Name())
				}
				approved = false
			case aerr != nil:
				// A genuine host-primitive failure (publish/transport) — fail closed,
				// same as a publish error.
				_ = host.Halt(ctx, "approval await failed: "+aerr.Error())
				return finish(Halt, "approval await failed", h.Name())
			}
			audit = append(audit, AuditRecord{
				Kind: "approval_resolved",
				Fields: map[string]any{
					"approvalKind": dec.Approval.Kind,
					"approved":     approved,
					"approver":     by,
				},
			})
			if !approved {
				dec.Verdict = Deny
				if dec.Reason == "" {
					dec.Reason = "approval denied or timed out"
				}
			}
			// approved ⇒ fall through with the hook's original (Allow) verdict
		}
		switch dec.Verdict {
		case Deny:
			def = dec.Definition
			return finish(Deny, dec.Reason, h.Name())
		case Halt:
			def = dec.Definition
			// Effect delivery is best-effort: a Notify/SetStatus/Audit failure must not
			// abort the gate decision. The generic executor has no logger; Hosts log
			// their own delivery failures (see Host contract).
			_ = host.Halt(ctx, dec.Reason)
			return finish(Halt, dec.Reason, h.Name())
		}
	}
	return finish(Allow, "", "")
}

// safeEval runs h.Eval, converting a panic into a fail-closed Halt Decision.
func safeEval(ctx context.Context, h Hook, in Input) (dec Decision) {
	defer func() {
		if r := recover(); r != nil {
			dec = Decision{Verdict: Halt, Reason: fmt.Sprintf("hook %q panic: %v", h.Name(), r)}
		}
	}()
	return h.Eval(ctx, in)
}
