package plans

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// Sentinel errors returned by Update. update_plan converts these into
// IsError tool results with actionable text.
var (
	ErrInvalidName             = errors.New("plan name is required and must be ≤64 chars")
	ErrInvalidItemID           = errors.New("item id is required and must be ≤64 chars")
	ErrDuplicateItemID         = errors.New("item ids must be unique within a plan")
	ErrInvalidStatus           = errors.New(`status must be one of "pending", "in_progress", "done", "error"`)
	ErrMultipleInProgress      = errors.New("at most one item may have status \"in_progress\"")
	ErrParentItemWithoutPlan   = errors.New("parent_item is set without parent_plan; either both or neither")
	ErrIllegalStatusRegression = errors.New(`status transitions out of terminal statuses are not allowed (done and error are terminal, including done↔error); remove the item and re-add with a new id to redo work`)
)

// noteVersion is the v field on the wrapped system_note for plans.
// Bump and add a migration in ReplayNote when the data shape changes.
const noteVersion = 1

// nowFunc is overridable for deterministic test timestamps.
var nowFunc = func() time.Time { return time.Now().UTC() }

// Store is the per-session in-memory plan registry.
type Store struct {
	mu    sync.RWMutex
	plans map[string]*Plan

	deps state.Deps
}

// NewStore is the factory invoked by the Kind. Holds onto deps for use
// in Update / replay.
func NewStore(deps state.Deps) *Store {
	return &Store{
		plans: map[string]*Plan{},
		deps:  deps,
	}
}

// Kind implements tool.StateStore.
func (*Store) Kind() string { return "plans" }

// Get returns a copy of the plan stored under name. (false, _) when
// no such plan exists.
func (s *Store) Get(name string) (Plan, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.plans[name]
	if !ok {
		return Plan{}, false
	}
	return clonePlan(p), true
}

// All returns a sorted slice of every plan currently stored. Test /
// debug helper; production code should not iterate.
func (s *Store) All() []Plan {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Plan, 0, len(s.plans))
	for _, p := range s.plans {
		out = append(out, clonePlan(p))
	}
	return out
}

// InProgress returns a copy of every plan that currently has an item in the
// in_progress state, sorted by name. The runner uses it to target plan
// activity (paused/active) echoes at only the plans whose rendering shows an
// active hourglass.
func (s *Store) InProgress() []Plan {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Plan
	for _, p := range s.plans {
		for _, it := range p.Items {
			if it.Status == StatusInProgress {
				out = append(out, clonePlan(p))
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Update applies items as the *full* new state of the named plan.
// See the spec (`Component 2 → Update semantics`) for the exact ordering.
//
// Returns ErrXxx for validation errors; otherwise returns the
// UpdateResult containing the diff and any in_progress info.
func (s *Store) Update(ctx context.Context, name string, parent ParentRef, c Content) (UpdateResult, error) {
	items := c.Items
	if name == "" || len(name) > 64 {
		return UpdateResult{}, ErrInvalidName
	}
	if parent.Item != "" && parent.Plan == "" {
		return UpdateResult{}, ErrParentItemWithoutPlan
	}
	if err := validateItems(items); err != nil {
		return UpdateResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.plans[name]
	diff, err := computeDiff(existing, items)
	if err != nil {
		return UpdateResult{}, err
	}

	// Build the new plan (in-flight; not yet committed to s.plans).
	newPlan := &Plan{
		Name:       name,
		ParentPlan: parent.Plan,
		ParentItem: parent.Item,
		Items:      append([]Item(nil), items...),
		Phases:     append([]Phase(nil), c.Phases...),
		UpdatedAt:  nowFunc(),
	}

	// Apply Begin/SetParent for pending → in_progress, Close for
	// in_progress → done. Operations succeeding before persistence is
	// the documented orphan window.
	var inProgress *InProgressRef
	for i := range newPlan.Items {
		it := &newPlan.Items[i]
		oldStatus := previousStatus(existing, it.ID)

		switch {
		case oldStatus != StatusInProgress && it.Status == StatusInProgress:
			if s.deps.Operations == nil {
				return UpdateResult{}, errors.New("plans.Update: Deps.Operations is nil; cannot auto-open operation")
			}
			op := s.deps.Operations.Begin(it.Label)
			s.deps.Operations.SetParent(op.ID, &tool.OperationParent{
				PlanItem: &tool.PlanItemRef{Plan: name, Item: it.ID},
			})
			it.OperationID = op.ID
			inProgress = &InProgressRef{ItemID: it.ID, OperationID: op.ID}

		case oldStatus == StatusInProgress && it.Status == StatusDone:
			oldItem, _ := existing.FindItem(it.ID)
			it.OperationID = oldItem.OperationID // keep it for audit
			if s.deps.Operations != nil && oldItem.OperationID != "" {
				s.deps.Operations.Close(oldItem.OperationID)
			}

		case oldStatus == StatusInProgress && it.Status == StatusError:
			oldItem, _ := existing.FindItem(it.ID)
			it.OperationID = oldItem.OperationID // keep for audit
			if s.deps.Operations != nil && oldItem.OperationID != "" {
				s.deps.Operations.Close(oldItem.OperationID)
			}

		case oldStatus == StatusInProgress && it.Status == StatusInProgress:
			// status didn't actually change — preserve the operation_id.
			if oldItem, ok := existing.FindItem(it.ID); ok {
				it.OperationID = oldItem.OperationID
				inProgress = &InProgressRef{ItemID: it.ID, OperationID: oldItem.OperationID}
			}

		default:
			// pending → pending, pending → done (skip), or done → done.
			// Keep prior OperationID if any, for stable audit reads.
			if existing != nil {
				if oldItem, ok := existing.FindItem(it.ID); ok {
					it.OperationID = oldItem.OperationID
				}
			}
		}
	}

	deletePlan := len(items) == 0

	// Persist the wrapped system_note BEFORE mutating s.plans.
	if err := s.persistNote(ctx, deletePlan, newPlan, name); err != nil {
		// Operations Begin'd above are now harmless orphans (un-Closed).
		return UpdateResult{}, fmt.Errorf("persist plan: %w", err)
	}

	// Commit.
	if deletePlan {
		// Close any still-open operations referenced by removed items.
		if existing != nil && s.deps.Operations != nil {
			for _, it := range existing.Items {
				if it.Status == StatusInProgress && it.OperationID != "" {
					s.deps.Operations.Close(it.OperationID)
				}
			}
		}
		delete(s.plans, name)
	} else {
		s.plans[name] = newPlan
	}

	res := UpdateResult{
		PlanName:   name,
		Diff:       diff,
		InProgress: inProgress,
	}
	return res, nil
}

// ReplayNote rebuilds the store from a wrapped system_note's data field.
// Implements tool.StateStore.
func (s *Store) ReplayNote(payload json.RawMessage) error {
	var n struct {
		Op   string `json:"op"`
		Plan *Plan  `json:"plan,omitempty"`
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(payload, &n); err != nil {
		return fmt.Errorf("replay plan note: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch n.Op {
	case "upsert":
		if n.Plan == nil || n.Plan.Name == "" {
			return errors.New("replay plan note: upsert missing plan/name")
		}
		s.plans[n.Plan.Name] = n.Plan
	case "delete":
		if n.Name == "" {
			return errors.New("replay plan note: delete missing name")
		}
		delete(s.plans, n.Name)
	default:
		return fmt.Errorf("replay plan note: unknown op %q", n.Op)
	}
	return nil
}

func (s *Store) persistNote(ctx context.Context, deletePlan bool, plan *Plan, name string) error {
	if s.deps.AppendSystemNote == nil {
		return nil // no-op in test fixtures / kubectl mode
	}
	var data map[string]any
	if deletePlan {
		data = map[string]any{"op": "delete", "name": name}
	} else {
		// Marshal/unmarshal to convert *Plan into a plain map shape so
		// AppendSystemNote's signature works without knowing about Plan.
		raw, err := json.Marshal(plan)
		if err != nil {
			return fmt.Errorf("marshal plan: %w", err)
		}
		var planMap map[string]any
		if err := json.Unmarshal(raw, &planMap); err != nil {
			return fmt.Errorf("unmarshal plan map: %w", err)
		}
		data = map[string]any{"op": "upsert", "plan": planMap}
	}
	wrapped := map[string]any{
		"kind": "plans",
		"v":    noteVersion,
		"data": data,
	}
	return s.deps.AppendSystemNote(ctx, wrapped)
}

func validateItems(items []Item) error {
	seen := map[string]struct{}{}
	inProgress := 0
	for _, it := range items {
		if it.ID == "" || len(it.ID) > 64 {
			return ErrInvalidItemID
		}
		if _, dup := seen[it.ID]; dup {
			return ErrDuplicateItemID
		}
		seen[it.ID] = struct{}{}
		if !it.Status.IsAgentSettable() {
			return ErrInvalidStatus
		}
		if it.Status == StatusInProgress {
			inProgress++
		}
	}
	if inProgress > 1 {
		return ErrMultipleInProgress
	}
	return nil
}

func previousStatus(existing *Plan, id string) Status {
	if existing == nil {
		return ""
	}
	if it, ok := existing.FindItem(id); ok {
		return it.Status
	}
	return ""
}

func computeDiff(existing *Plan, newItems []Item) (Diff, error) {
	var diff Diff
	oldByID := map[string]Item{}
	if existing != nil {
		for _, it := range existing.Items {
			oldByID[it.ID] = it
		}
	}
	newByID := map[string]struct{}{}
	for _, it := range newItems {
		newByID[it.ID] = struct{}{}
		old, hadOld := oldByID[it.ID]
		if !hadOld {
			diff.Added = append(diff.Added, it.ID)
			continue
		}
		if old.Label != it.Label {
			diff.Renamed = append(diff.Renamed, RenameDiff{ID: it.ID, From: old.Label, To: it.Label})
		}
		if old.Status != it.Status {
			if isIllegalRegression(old.Status, it.Status) {
				return Diff{}, ErrIllegalStatusRegression
			}
			diff.StatusChanged = append(diff.StatusChanged, StatusChangeDiff{ID: it.ID, From: old.Status, To: it.Status})
		}
	}
	for id := range oldByID {
		if _, present := newByID[id]; !present {
			diff.Removed = append(diff.Removed, id)
		}
	}
	return diff, nil
}

// isIllegalRegression rejects status transitions that violate the
// terminal-status invariant. Done, error, and stopped are all terminal;
// the agent must add a new item rather than flip a terminal item back
// or sideways. We also forbid done→error to prevent rewriting a
// successful step as failed after the fact. Stopped is system-only (the
// agent cannot even submit it via update_plan), but the guard is here
// as a defensive backstop for unexpected in-memory paths.
func isIllegalRegression(from, to Status) bool {
	if from == StatusDone && to != StatusDone {
		return true
	}
	if from == StatusError && to != StatusError {
		return true
	}
	if from == StatusStopped && to != StatusStopped {
		return true
	}
	return false
}

// MarkStopped sets every non-terminal item (pending, in_progress) across all
// plans to StatusStopped, closes any open operations, persists one upsert note
// per changed plan, and returns the changed plans for channel publication.
// Idempotent: done/error/already-stopped items are skipped. Called by the
// runner on interrupted termination (SIGTERM / admin-kill / crash).
func (s *Store) MarkStopped(ctx context.Context) ([]Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var changed []Plan
	for name, p := range s.plans {
		// Build a modified copy; only commit after persist succeeds so a
		// persistence failure leaves the in-memory state consistent.
		newItems := append([]Item(nil), p.Items...)
		var dirty bool
		for i := range newItems {
			if newItems[i].Status.IsTerminal() {
				continue
			}
			if newItems[i].Status == StatusInProgress {
				// Mirror the in_progress→terminal close in Update.
				if s.deps.Operations != nil && newItems[i].OperationID != "" {
					s.deps.Operations.Close(newItems[i].OperationID)
				}
			}
			newItems[i].Status = StatusStopped
			dirty = true
		}
		if !dirty {
			continue
		}
		newPlan := *p
		newPlan.Items = newItems
		newPlan.UpdatedAt = nowFunc()
		if err := s.persistNote(ctx, false, &newPlan, name); err != nil {
			return nil, err
		}
		s.plans[name] = &newPlan
		changed = append(changed, newPlan)
	}
	return changed, nil
}

// clonePlan returns a copy safe to hand outside the store's lock.
//
// Every slice field must be copied explicitly: `out := *p` copies slice
// HEADERS, so an un-cloned slice would still alias the store's backing array
// and let a caller mutate committed state through a "copy". Phases is cloned
// for exactly that reason.
func clonePlan(p *Plan) Plan {
	out := *p
	out.Items = append([]Item(nil), p.Items...)
	out.Phases = append([]Phase(nil), p.Phases...)
	return out
}
