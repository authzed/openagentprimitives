package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// interruptibleReason reports whether a single tool can be truthfully
// cancelled in flight, and if not, why. A tool is interruptible iff it
// implements tool.Cancellable (declares in-flight cancel support) and
// mutates no durable state (StateImpact stateless or readonly). Shared by
// isInterruptibleTool (a single registry entry, consulted by fire()) and
// currentInterruptibility (the whole live in-flight batch) so callers can
// never drift on what counts as interruptible.
func interruptibleReason(t tool.Tool) (bool, string) {
	if _, ok := t.(tool.Cancellable); !ok {
		return false, fmt.Sprintf("%s can't be interrupted", t.Name())
	}
	switch t.Permission().StateImpact {
	case authz.Stateless, authz.Readonly:
		return true, ""
	default:
		return false, fmt.Sprintf("%s is changing state", t.Name())
	}
}

// isInterruptibleTool reports whether t can be truthfully cancelled in
// flight (Cancellable AND stateless/readonly), discarding the reason.
func isInterruptibleTool(t tool.Tool) bool {
	ok, _ := interruptibleReason(t)
	return ok
}

var errInterruptedByUser = errors.New("interrupted by user")

// interruptEntry pairs a live in-flight tool with its cancel func, so the
// registry can decide at fire() time whether cancelling it is truthful
// (Cancellable AND stateless/readonly) without a caller-supplied snapshot.
type interruptEntry struct {
	tool   tool.Tool
	cancel context.CancelCauseFunc
}

// interruptRegistry holds the live in-flight tools + cancel funcs so
// Loop.Interrupt can read the current state directly instead of trusting a
// caller-supplied snapshot, and cancel the interruptible subset with the
// errInterruptedByUser cause. Populated by the dispatch fan-out (per
// tool_use) and the per-turn LLM send; fired by Interrupt. Zero value is
// ready to use.
type interruptRegistry struct {
	mu             sync.Mutex
	cancels        map[string]interruptEntry
	providerCancel context.CancelCauseFunc
	requested      bool
}

func (r *interruptRegistry) registerTool(id string, t tool.Tool, c context.CancelCauseFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancels == nil {
		r.cancels = map[string]interruptEntry{}
	}
	r.cancels[id] = interruptEntry{tool: t, cancel: c}
}

// currentTools returns a snapshot of the tools currently registered as
// in-flight, taken under the registry lock. The returned slice is a copy —
// callers may safely inspect it, or call back into other subsystems (the
// approval orchestrator, tool.Cancellable.Cancel), without holding r.mu.
func (r *interruptRegistry) currentTools() []tool.Tool {
	r.mu.Lock()
	defer r.mu.Unlock()
	tools := make([]tool.Tool, 0, len(r.cancels))
	for _, e := range r.cancels {
		tools = append(tools, e.tool)
	}
	return tools
}

func (r *interruptRegistry) deregisterTool(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, id)
}

func (r *interruptRegistry) setProvider(c context.CancelCauseFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providerCancel = c
}

func (r *interruptRegistry) clearProvider() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providerCancel = nil
}

// hasProvider reports whether the LLM is currently mid-send (a provider
// cancel func is registered). Guarded by the registry mutex — never read
// r.providerCancel directly from outside this file.
func (r *interruptRegistry) hasProvider() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providerCancel != nil
}

// fire cancels every registered tool ctx whose tool is interruptible-kind
// (Cancellable AND stateless/readonly — see isInterruptibleTool) and the
// provider ctx, both with the errInterruptedByUser cause, and records that an
// interrupt was requested. Skipping a stateful entry is defense-in-depth:
// Loop.Interrupt (currentInterruptibility) already refuses to call fire() when
// any live tool is stateful, but fire() itself must never truthfully cancel a
// durable-state mutation, whatever the caller.
//
// requested is set unconditionally, even when nothing was cancelled:
// takeRequested/drainInbox are idempotent, so the extra no-op drain-at-yield is
// benign.
func (r *interruptRegistry) fire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requested = true
	for _, e := range r.cancels {
		if isInterruptibleTool(e.tool) {
			e.cancel(errInterruptedByUser)
		}
	}
	if r.providerCancel != nil {
		r.providerCancel(errInterruptedByUser)
	}
}

// takeRequested returns whether an interrupt was requested since the last call,
// clearing the flag. Read by the loop after dispatch / after a send error.
func (r *interruptRegistry) takeRequested() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.requested
	r.requested = false
	return was
}

// drainAfterInterrupt is the shared post-interrupt yield handler for both the
// cancelled-Provider.Send site and the post-dispatch tool-batch site. If an
// interrupt was requested since it was last consumed, it drains the held inbox
// at this boundary (via drainHeldAtYield, so the "Picking up N messages" notify
// fires exactly as at the ordinary yield sites) and reports cont=true so the
// caller splices the drained turns in and continues instead of treating the
// interruption as a failure. cont=false, err=nil means no interrupt was pending.
func (l *Loop) drainAfterInterrupt(ctx context.Context, nextIndex int) ([]memory.Turn, bool, error) {
	if !l.interrupts.takeRequested() {
		return nil, false, nil
	}
	placed, _, err := l.drainHeldAtYield(ctx, nextIndex)
	return placed, true, err
}

// sessionRef returns the session key in the same "namespace/name" format the
// approval Orchestrator uses to key pending awaits (see
// runnerHost.AwaitDecision in host_approval.go, which builds
// approval.Request.SessionRef identically). This MUST match exactly, or
// PendingForSession / PendingRequestIDsForSession return nothing for this
// session and Interrupt's approval-deny step silently does nothing.
func (l *Loop) sessionRef() string {
	return l.SessionKey.Namespace + "/" + l.SessionKey.Name
}

// InterruptOutcome is the result of a Loop.Interrupt call: whether in-flight
// work was actually cancelled, or a reason it could not be.
type InterruptOutcome struct {
	Interrupted bool
	Reason      string
}

// currentInterruptibility reports whether the work currently in flight — per
// the LIVE interrupt registry, not a caller-supplied snapshot — can be
// truthfully cancelled, and if not, a reason naming the blocker. The registered
// tools are snapshotted under the registry lock (currentTools), then evaluated
// outside it: hasProvider and l.Approval.PendingForSession call into the
// approval orchestrator and must never run while holding interruptRegistry.mu,
// or a concurrent registerTool/fire deadlocks against it.
func (l *Loop) currentInterruptibility() (bool, string) {
	tools := l.interrupts.currentTools()
	for _, t := range tools {
		if ok, reason := interruptibleReason(t); !ok {
			return false, reason
		}
	}

	midSend := l.interrupts.hasProvider()
	pending := 0
	if l.Approval != nil {
		pending = l.Approval.PendingForSession(l.sessionRef())
	}

	if midSend || pending > 0 || len(tools) > 0 {
		return true, ""
	}
	return false, "nothing is running"
}

// cancelInterruptibleTools calls Cancel (best-effort) on every t in tools that
// is truthfully interruptible (Cancellable AND stateless/readonly — see
// isInterruptibleTool), skipping any stateful entry. The guard is
// defense-in-depth: Interrupt already refuses to reach this point when the live
// snapshot it checked contained a stateful tool, but this is a fresh, separate
// snapshot, and a stateful sibling registering in between could otherwise land
// here. Skipping it keeps "a readwrite sibling survives an interrupt" true even
// under that race. Errors are logged, not returned — best-effort teardown beyond
// the ctx-cancel.
func cancelInterruptibleTools(ctx context.Context, tools []tool.Tool, sessionRef string) {
	for _, t := range tools {
		if !isInterruptibleTool(t) {
			continue // never tear down a stateful tool (mirrors fire())
		}
		if c, ok := t.(tool.Cancellable); ok {
			if err := c.Cancel(ctx); err != nil {
				slog.Default().Info("tool Cancel errored during interrupt (best-effort)",
					"session", sessionRef, "tool", t.Name(), "err", err.Error())
			}
		}
	}
}

// Interrupt cancels the interruptible in-flight work (an MCP/sidecar tool
// batch, a mid-stream LLM call, and/or a pending approval) so the runner can
// jump to the queued messages. It reads the LIVE interrupt registry
// (currentInterruptibility) rather than a caller-supplied snapshot: the
// channel-triggered interrupt has no visibility into what the loop currently has
// dispatched, only the registry does. Returns Interrupted=false with a reason
// when the current work cannot be truthfully cancelled; nothing is fired then.
func (l *Loop) Interrupt(ctx context.Context) InterruptOutcome {
	ok, reason := l.currentInterruptibility()
	if !ok {
		return InterruptOutcome{Interrupted: false, Reason: reason}
	}

	// Deny every pending approval so the awaiting goroutine unblocks.
	if l.Approval != nil {
		for _, reqID := range l.Approval.PendingRequestIDsForSession(l.sessionRef()) {
			l.Approval.DeliverDecision(reqID, approval.Decision{Approved: false, Reason: "interrupted by user"})
		}
	}
	// Best-effort per-tool teardown beyond the ctx-cancel (e.g. a sandbox CR
	// delete). Re-snapshot rather than reuse currentInterruptibility's: this runs
	// after the approval-deny step, outside the registry lock, and a fresh
	// snapshot is cheap against a concurrent register/deregister in between.
	cancelInterruptibleTools(ctx, l.interrupts.currentTools(), l.sessionRef())
	l.interrupts.fire()
	return InterruptOutcome{Interrupted: true}
}
