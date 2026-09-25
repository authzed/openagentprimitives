package pipeline_test

import (
	"context"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// fakeHook returns a scripted Decision and records that it ran.
type fakeHook struct {
	name   string
	points []pipeline.Point
	decide func(pipeline.Input) pipeline.Decision
	ran    bool
}

func (f *fakeHook) Name() string             { return f.name }
func (f *fakeHook) Points() []pipeline.Point { return f.points }
func (f *fakeHook) Eval(_ context.Context, in pipeline.Input) pipeline.Decision {
	f.ran = true
	if f.decide != nil {
		return f.decide(in)
	}
	return pipeline.Decision{} // Allow
}

// fakeHost records every primitive call; approval result is scriptable.
type fakeHost struct {
	mu            sync.Mutex
	approvals     []pipeline.ApprovalAsk
	notices       []pipeline.Notice
	statuses      []pipeline.StatusUpdate
	audits        []pipeline.AuditRecord
	halted        bool
	haltReason    string
	approveResult bool
	approveErr    error
	awaitErr      error
	timedOut      bool
	// awaitHook, when set, runs inside AwaitDecision — used to hold a decision
	// open so a second caller genuinely races the first rather than arriving
	// after it resolved.
	awaitHook func()
}

// publishedAsks returns the asks published so far, under the lock.
func (h *fakeHost) publishedAsks() []pipeline.ApprovalAsk {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]pipeline.ApprovalAsk, len(h.approvals))
	copy(out, h.approvals)
	return out
}

func (h *fakeHost) PublishApproval(_ context.Context, a pipeline.ApprovalAsk) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.approvals = append(h.approvals, a)
	return "req-1", h.approveErr
}
func (h *fakeHost) AwaitDecision(_ context.Context, _ string, _ time.Duration) (bool, string, bool, error) {
	h.mu.Lock()
	hook := h.awaitHook
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	return h.approveResult, "approver", h.timedOut, h.awaitErr
}
func (h *fakeHost) Notify(_ context.Context, n pipeline.Notice) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notices = append(h.notices, n)
	return nil
}
func (h *fakeHost) SetStatus(_ context.Context, s pipeline.StatusUpdate) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statuses = append(h.statuses, s)
	return nil
}
func (h *fakeHost) Halt(_ context.Context, reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.halted = true
	h.haltReason = reason
	return nil
}
func (h *fakeHost) Audit(_ context.Context, recs []pipeline.AuditRecord) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.audits = append(h.audits, recs...)
	return nil
}

// Compile-time assertions that the fakes satisfy the interfaces.
var _ pipeline.Hook = (*fakeHook)(nil)
var _ pipeline.Host = (*fakeHost)(nil)
