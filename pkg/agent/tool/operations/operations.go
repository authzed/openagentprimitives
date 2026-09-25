// Package operations is the per-session audit-trail registry used by
// new_operation (meta tool) and the sandbox dispatcher to enforce that
// every external tool call is justified by a logical operation.
//
// The registry lives entirely in-process. Operations are scoped to a single
// runner-loop lifetime; on runner restart, mid-flight operations need to be
// re-created by the model. Future work may persist operations alongside
// memory turns to survive restarts.
package operations

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// Registry is the thread-safe in-process implementation of
// tool.OperationRegistry.
type Registry struct {
	mu    sync.RWMutex
	ops   map[string]*tool.Operation
	now   func() time.Time
	newID func() string
	// root is the ID of the session-root operation, "" until Root mints it.
	root string
}

// Compile-time check that Registry satisfies the interface used by tools.
var _ tool.OperationRegistry = (*Registry)(nil)

// New constructs an empty registry. now defaults to time.Now if nil.
//
// newID defaults to generateID if nil, which is what every binary passes: the
// seam exists so a whole-session REPLAY can hand back the very operation ids
// the captured run minted, keeping the recorded arguments literal and correct.
// It is deliberately not reachable from a tool argument — an id the model could
// choose is an id it could collide with another operation on.
//
// Only Begin draws from it. See Root for why the session root does not.
func New(now func() time.Time, newID func() string) *Registry {
	if now == nil {
		now = time.Now
	}
	if newID == nil {
		newID = generateID
	}
	return &Registry{ops: map[string]*tool.Operation{}, now: now, newID: newID}
}

func (r *Registry) Begin(description string) tool.Operation {
	id := r.newID()
	op := &tool.Operation{ID: id, Description: description, CreatedAt: r.now()}
	r.mu.Lock()
	r.ops[id] = op
	r.mu.Unlock()
	return *op
}

// Root implements tool.OperationRegistry. Mints on first call, then returns
// the same operation forever.
//
// Double-checked under the write lock rather than the read lock alone: two
// goroutines resolving an ambient operation concurrently (a meta tool on the
// loop goroutine and a widget app-tool on the NATS goroutine) would otherwise
// both observe "no root" and mint two, which is precisely the scattering the
// root exists to prevent.
//
// Mints with generateID rather than New's newID, and that asymmetry is the
// point of the seam rather than an omission. The seam reproduces the ids a
// captured run handed to the MODEL; the root's id is never one of those —
// resolveAmbientOperation mints it internally and no tool result carries it —
// so a capture cannot observe it, cannot record it, and must not be charged for
// it. Charging it would consume an entry the capture never wrote, on the first
// unattributed call, whose position in the run depends on which tools the model
// happened to reach for.
func (r *Registry) Root() tool.Operation {
	r.mu.RLock()
	if r.root != "" {
		op := r.ops[r.root]
		r.mu.RUnlock()
		return cloneOp(op)
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.root != "" {
		return cloneOp(r.ops[r.root])
	}
	id := generateID()
	op := &tool.Operation{
		ID:          id,
		Description: rootDescription,
		CreatedAt:   r.now(),
		Root:        true,
	}
	r.ops[id] = op
	r.root = id
	return *op
}

// rootDescription is what an audit reader sees for calls the agent never
// attributed. Phrased as the fallback it is, so a reader is not left believing
// the agent declared an operation by this name.
const rootDescription = "session (calls not attributed to a declared operation)"

func (r *Registry) Get(id string) (tool.Operation, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	op, ok := r.ops[id]
	if !ok {
		return tool.Operation{}, false
	}
	return cloneOp(op), true
}

func (r *Registry) RecordCall(id string, call tool.OperationCall) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.ops[id]
	if !ok {
		return -1, false
	}
	if call.At.IsZero() {
		call.At = r.now()
	}
	op.Calls = append(op.Calls, call)
	return len(op.Calls) - 1, true
}

func (r *Registry) CompleteCall(id string, index int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.ops[id]
	if !ok {
		return false
	}
	if index < 0 || index >= len(op.Calls) {
		return false
	}
	op.Calls[index].CompletedAt = r.now()
	return true
}

func (r *Registry) All() []tool.Operation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]tool.Operation, 0, len(r.ops))
	for _, op := range r.ops {
		out = append(out, cloneOp(op))
	}
	return out
}

func (r *Registry) SetParent(id string, p *tool.OperationParent) bool {
	if p == nil {
		panic("operations.SetParent: parent must not be nil; pass concrete OperationID or PlanItem")
	}
	hasOp := p.OperationID != ""
	hasItem := p.PlanItem != nil
	if hasOp == hasItem {
		panic("operations.SetParent: parent must have exactly one of OperationID or PlanItem")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.ops[id]
	if !ok {
		return false
	}
	// Defensive copy of the PlanItem ref — caller may mutate after the
	// call; we own a stable snapshot in the registry.
	clone := &tool.OperationParent{OperationID: p.OperationID}
	if p.PlanItem != nil {
		clone.PlanItem = &tool.PlanItemRef{Plan: p.PlanItem.Plan, Item: p.PlanItem.Item}
	}
	op.Parent = clone
	return true
}

func (r *Registry) Close(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.ops[id]
	if !ok {
		return false
	}
	op.Closed = true
	return true
}

// cloneOp deep-copies an Operation so external callers can't mutate the
// registry's slice of Calls under our lock.
func cloneOp(op *tool.Operation) tool.Operation {
	out := *op
	if op.Parent != nil {
		p := *op.Parent
		if op.Parent.PlanItem != nil {
			pi := *op.Parent.PlanItem
			p.PlanItem = &pi
		}
		out.Parent = &p
	}
	if len(op.Calls) > 0 {
		out.Calls = make([]tool.OperationCall, len(op.Calls))
		copy(out.Calls, op.Calls)
	}
	return out
}

// generateID produces a short, URL-safe operation identifier. Collisions in
// a single session are vanishingly unlikely.
func generateID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "op-" + hex.EncodeToString(b[:])
}
