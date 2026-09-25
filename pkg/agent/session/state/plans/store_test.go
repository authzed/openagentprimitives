package plans_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
)

type capturedNote struct {
	content map[string]any
	err     error
}

type capturingDeps struct {
	notes []capturedNote
	fail  error
}

func (c *capturingDeps) append(_ context.Context, content map[string]any) error {
	c.notes = append(c.notes, capturedNote{content: content, err: c.fail})
	return c.fail
}

// newStore builds a plans.Store with a real operations.Registry and a
// capturing AppendSystemNote so tests can assert both the audit and the
// persistence side effects.
func newStore(t *testing.T) (*plans.Store, *operations.Registry, *capturingDeps) {
	t.Helper()
	ops := operations.New(nil, nil)
	cap := &capturingDeps{}
	deps := state.Deps{Operations: ops, AppendSystemNote: cap.append}
	return plans.NewStore(deps), ops, cap
}

func TestUpdate_RejectsEmptyName(t *testing.T) {
	s, _, _ := newStore(t)
	_, err := s.Update(context.Background(), "", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusPending},
	}})

	require.ErrorIs(t, err, plans.ErrInvalidName)
}

func TestUpdate_RejectsDuplicateItemIDs(t *testing.T) {
	s, _, _ := newStore(t)
	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusPending},
		{ID: "a", Label: "B", Status: plans.StatusPending},
	}})

	require.ErrorIs(t, err, plans.ErrDuplicateItemID)
}

func TestUpdate_RejectsEmptyItemID(t *testing.T) {
	s, _, _ := newStore(t)
	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "", Label: "A", Status: plans.StatusPending},
	}})

	require.ErrorIs(t, err, plans.ErrInvalidItemID)
}

func TestUpdate_RejectsInvalidStatus(t *testing.T) {
	s, _, _ := newStore(t)
	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.Status("blocked")},
	}})

	require.ErrorIs(t, err, plans.ErrInvalidStatus)
}

func TestUpdate_RejectsTwoInProgress(t *testing.T) {
	s, _, _ := newStore(t)
	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusInProgress},
		{ID: "b", Label: "B", Status: plans.StatusInProgress},
	}})

	require.ErrorIs(t, err, plans.ErrMultipleInProgress)
}

func TestUpdate_RejectsItemWithoutPlan(t *testing.T) {
	s, _, _ := newStore(t)
	_, err := s.Update(context.Background(), "main", plans.ParentRef{Plan: "", Item: "x"}, plans.Content{Items: []plans.Item{{ID: "a", Label: "A", Status: plans.StatusPending}}})

	require.ErrorIs(t, err, plans.ErrParentItemWithoutPlan)
}

func TestUpdate_FirstCallCreatesPlanAndPersists(t *testing.T) {
	s, _, cap := newStore(t)
	res, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "Step A", Status: plans.StatusPending},
		{ID: "b", Label: "Step B", Status: plans.StatusPending},
	}})

	require.NoError(t, err)
	require.Equal(t, "main", res.PlanName)
	require.Nil(t, res.InProgress)
	require.ElementsMatch(t, []string{"a", "b"}, res.Diff.Added)
	require.Empty(t, res.Diff.Removed)
	require.Empty(t, res.Diff.Renamed)
	require.Empty(t, res.Diff.StatusChanged)

	got, ok := s.Get("main")
	require.True(t, ok)
	require.Equal(t, "main", got.Name)
	require.Len(t, got.Items, 2)
	require.False(t, got.UpdatedAt.IsZero())

	// Persisted system_note has the framework wrapper.
	require.Len(t, cap.notes, 1)
	note := cap.notes[0].content
	require.Equal(t, "plans", note["kind"])
	require.EqualValues(t, 1, note["v"])
	data, ok := note["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "upsert", data["op"])
}

func TestUpdate_PendingToInProgress_BeginsOperation(t *testing.T) {
	s, ops, _ := newStore(t)

	// First call: create plan with both pending.
	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "Step A", Status: plans.StatusPending},
		{ID: "b", Label: "Step B", Status: plans.StatusPending},
	}})

	require.NoError(t, err)

	// Second call: flip a to in_progress.
	res, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "Step A", Status: plans.StatusInProgress},
		{ID: "b", Label: "Step B", Status: plans.StatusPending},
	}})

	require.NoError(t, err)

	require.NotNil(t, res.InProgress)
	require.Equal(t, "a", res.InProgress.ItemID)
	require.NotEmpty(t, res.InProgress.OperationID)

	// Operation was Begun and SetParent'd to (main, a).
	op, ok := ops.Get(res.InProgress.OperationID)
	require.True(t, ok)
	require.Equal(t, "Step A", op.Description)
	require.False(t, op.Closed)
	require.NotNil(t, op.Parent)
	require.NotNil(t, op.Parent.PlanItem)
	require.Equal(t, "main", op.Parent.PlanItem.Plan)
	require.Equal(t, "a", op.Parent.PlanItem.Item)

	// Diff captures the status change.
	require.Equal(t, []plans.StatusChangeDiff{
		{ID: "a", From: plans.StatusPending, To: plans.StatusInProgress},
	}, res.Diff.StatusChanged)

	// OperationID stored on the persisted item.
	got, _ := s.Get("main")
	itemA, _ := got.FindItem("a")
	require.Equal(t, res.InProgress.OperationID, itemA.OperationID)
}

func TestUpdate_InProgressToDone_ClosesOperation(t *testing.T) {
	s, ops, _ := newStore(t)

	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusInProgress},
	}})

	require.NoError(t, err)
	got, _ := s.Get("main")
	opID := got.Items[0].OperationID
	require.NotEmpty(t, opID)

	_, err = s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusDone},
	}})

	require.NoError(t, err)

	op, ok := ops.Get(opID)
	require.True(t, ok)
	require.True(t, op.Closed)
}

func TestUpdate_PendingToDoneSkip_AllowedNoOperation(t *testing.T) {
	s, ops, _ := newStore(t)

	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusPending},
	}})

	require.NoError(t, err)

	_, err = s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusDone},
	}})

	require.NoError(t, err)

	// No operation should have been created (no in_progress transition).
	require.Empty(t, ops.All())
}

func TestUpdate_DoneToInProgress_Rejected(t *testing.T) {
	s, _, _ := newStore(t)

	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusDone},
	}})

	require.NoError(t, err)

	_, err = s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusInProgress},
	}})

	require.ErrorIs(t, err, plans.ErrIllegalStatusRegression)
}

func TestUpdate_DoneToPending_Rejected(t *testing.T) {
	s, _, _ := newStore(t)

	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusDone},
	}})

	require.NoError(t, err)

	_, err = s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusPending},
	}})

	require.ErrorIs(t, err, plans.ErrIllegalStatusRegression)
}

func TestUpdate_AddRemoveRename(t *testing.T) {
	s, _, _ := newStore(t)
	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "Old A", Status: plans.StatusPending},
		{ID: "b", Label: "B", Status: plans.StatusPending},
	}})

	require.NoError(t, err)

	res, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "New A", Status: plans.StatusPending},
		{ID: "c", Label: "C", Status: plans.StatusPending},
	}})

	require.NoError(t, err)
	require.ElementsMatch(t, []string{"c"}, res.Diff.Added)
	require.ElementsMatch(t, []string{"b"}, res.Diff.Removed)
	require.Equal(t, []plans.RenameDiff{{ID: "a", From: "Old A", To: "New A"}}, res.Diff.Renamed)
}

func TestUpdate_EmptyItems_DeletesPlanAndClosesOpenOperations(t *testing.T) {
	s, ops, _ := newStore(t)

	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusInProgress},
	}})

	require.NoError(t, err)
	got, _ := s.Get("main")
	opID := got.Items[0].OperationID

	_, err = s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{}})
	require.NoError(t, err)

	_, ok := s.Get("main")
	require.False(t, ok, "plan should be deleted")
	op, ok := ops.Get(opID)
	require.True(t, ok)
	require.True(t, op.Closed, "open operation must be closed when plan is deleted")
}

func TestUpdate_PersistFailure_AbortsWithoutMutating(t *testing.T) {
	s, _, cap := newStore(t)

	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusPending},
	}})

	require.NoError(t, err)
	require.Len(t, cap.notes, 1)

	// Now simulate persistence failure.
	cap.fail = errors.New("disk full")
	_, err = s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusInProgress},
	}})

	require.Error(t, err)
	require.ErrorContains(t, err, "disk full")

	// Plan in-memory still reflects the *previous* (pending) state.
	got, ok := s.Get("main")
	require.True(t, ok)
	itemA, _ := got.FindItem("a")
	require.Equal(t, plans.StatusPending, itemA.Status)
}

func TestReplayNote_UpsertRebuildsPlan(t *testing.T) {
	s, _, _ := newStore(t)

	payload := []byte(`{
		"op":"upsert",
		"plan":{
			"name":"main",
			"items":[
				{"id":"a","label":"A","status":"in_progress","operation_id":"op-stale-123"},
				{"id":"b","label":"B","status":"pending"}
			],
			"updated_at":"2026-05-06T12:00:00Z"
		}
	}`)
	require.NoError(t, s.ReplayNote(payload))

	got, ok := s.Get("main")
	require.True(t, ok)
	require.Len(t, got.Items, 2)
	require.Equal(t, plans.StatusInProgress, got.Items[0].Status)
	require.Equal(t, "op-stale-123", got.Items[0].OperationID)
}

func TestReplayNote_DeleteRemovesPlan(t *testing.T) {
	s, _, _ := newStore(t)

	require.NoError(t, s.ReplayNote([]byte(`{
		"op":"upsert",
		"plan":{"name":"main","items":[{"id":"a","label":"A","status":"pending"}],"updated_at":"2026-05-06T12:00:00Z"}
	}`)))
	require.NoError(t, s.ReplayNote([]byte(`{"op":"delete","name":"main"}`)))

	_, ok := s.Get("main")
	require.False(t, ok)
}

func TestReplayNote_LastWriteWins(t *testing.T) {
	s, _, _ := newStore(t)

	require.NoError(t, s.ReplayNote([]byte(`{
		"op":"upsert",
		"plan":{"name":"main","items":[{"id":"a","label":"v1","status":"pending"}],"updated_at":"2026-05-06T12:00:00Z"}
	}`)))
	require.NoError(t, s.ReplayNote([]byte(`{
		"op":"upsert",
		"plan":{"name":"main","items":[{"id":"a","label":"v2","status":"pending"}],"updated_at":"2026-05-06T12:01:00Z"}
	}`)))

	got, _ := s.Get("main")
	require.Equal(t, "v2", got.Items[0].Label)
}

func TestReplayNote_DoesNotCallOperationsRegistry(t *testing.T) {
	s, ops, _ := newStore(t)

	// Replay items that *would* have triggered Begin in Update.
	require.NoError(t, s.ReplayNote([]byte(`{
		"op":"upsert",
		"plan":{"name":"main","items":[{"id":"a","label":"A","status":"in_progress","operation_id":"op-pre-restart"}],"updated_at":"2026-05-06T12:00:00Z"}
	}`)))

	// Operations registry stays empty — replay never calls Begin/Close.
	require.Empty(t, ops.All())
}

func TestReplayNote_MalformedReturnsError(t *testing.T) {
	s, _, _ := newStore(t)
	require.Error(t, s.ReplayNote([]byte(`{not json`)))
}

func TestUpdate_IsConcurrencySafe(t *testing.T) {
	s, _, _ := newStore(t)
	const N = 20
	done := make(chan struct{}, N)
	for i := 0; i < N; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
				{ID: "a", Label: "A", Status: plans.StatusPending},
			}})

		}()
	}
	for i := 0; i < N; i++ {
		<-done
	}
	got, ok := s.Get("main")
	require.True(t, ok)
	require.Len(t, got.Items, 1)
}

func TestStore_Update_AcceptsErrorStatusForNewItem(t *testing.T) {
	s, ops, _ := newStore(t)
	res, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusError},
	}})

	require.NoError(t, err)
	require.Contains(t, res.Diff.Added, "a")
	stored, ok := s.Get("main")
	require.True(t, ok)
	require.Equal(t, plans.StatusError, stored.Items[0].Status)
	require.Empty(t, stored.Items[0].OperationID, "pending→error path should not open an op")
	require.Empty(t, ops.All(), "no operation should have been created")
}

func TestStore_Update_InProgressToErrorClosesOp(t *testing.T) {
	s, ops, _ := newStore(t)

	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusInProgress},
	}})

	require.NoError(t, err)
	allOps := ops.All()
	require.Len(t, allOps, 1, "in_progress should Begin an op")
	opID := allOps[0].ID
	op, ok := ops.Get(opID)
	require.True(t, ok)
	require.False(t, op.Closed, "op should still be open before the error transition")

	_, err = s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusError},
	}})

	require.NoError(t, err)
	op, ok = ops.Get(opID)
	require.True(t, ok)
	require.True(t, op.Closed, "in_progress→error must close the op")

	stored, _ := s.Get("main")
	require.NotEmpty(t, stored.Items[0].OperationID, "OperationID is preserved for audit")
	require.Equal(t, opID, stored.Items[0].OperationID)
}

func TestStore_Update_RejectsTransitionsOutOfError(t *testing.T) {
	cases := []struct {
		name string
		next plans.Status
	}{
		{"error→pending", plans.StatusPending},
		{"error→in_progress", plans.StatusInProgress},
		{"error→done", plans.StatusDone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, _ := newStore(t)
			_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
				{ID: "a", Label: "A", Status: plans.StatusError},
			}})

			require.NoError(t, err)
			_, err = s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
				{ID: "a", Label: "A", Status: c.next},
			}})

			require.ErrorIs(t, err, plans.ErrIllegalStatusRegression,
				"transition out of error must be rejected")
		})
	}
}

func TestStore_Update_RejectsDoneToError(t *testing.T) {
	s, _, _ := newStore(t)
	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusDone},
	}})

	require.NoError(t, err)
	_, err = s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusError},
	}})

	require.ErrorIs(t, err, plans.ErrIllegalStatusRegression,
		"done→error must be rejected (don't rewrite successful items as failed)")
}

func TestStore_Update_DetailsOutputRoundtrip(t *testing.T) {
	s, _, _ := newStore(t)
	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusDone, Details: "why", Output: "result"},
	}})

	require.NoError(t, err)
	stored, _ := s.Get("main")
	require.Equal(t, "why", stored.Items[0].Details)
	require.Equal(t, "result", stored.Items[0].Output)
}

// also verify the wrapped note text round-trips through the framework
// dispatcher without needing custom plumbing.
func TestStore_AsStateStore_DispatchSystemNoteRoutes(t *testing.T) {
	// Clear first: this registers its own "plans" Kind to capture the Store
	// NewRegistry builds, and the package's init() already holds that name. The
	// restore hands the init-registered Kind back to the next test.
	t.Cleanup(state.ResetForTest())
	plansStoreCh := make(chan tool.StateStore, 1)
	state.Register(&fakeKindWithStore{
		name: "plans",
		newStoreFn: func(d state.Deps) tool.StateStore {
			s := plans.NewStore(d)
			plansStoreCh <- s
			return s
		},
	})
	r := state.NewRegistry(state.Deps{})
	plansStore := <-plansStoreCh

	wrapped := []byte(`{
		"kind":"plans","v":1,"data":{
			"op":"upsert",
			"plan":{"name":"main","items":[{"id":"x","label":"X","status":"pending"}],"updated_at":"2026-05-06T12:00:00Z"}
		}
	}`)
	matched, err := state.DispatchSystemNote(r, wrapped)
	require.True(t, matched)
	require.NoError(t, err)
	got, ok := plansStore.(*plans.Store).Get("main")
	require.True(t, ok)
	require.Equal(t, "X", got.Items[0].Label)

	// silence unused-import linter for json/state if test trims
	_ = json.RawMessage(nil)
}

type fakeKindWithStore struct {
	name       string
	newStoreFn func(state.Deps) tool.StateStore
}

func (k *fakeKindWithStore) Name() string                          { return k.name }
func (k *fakeKindWithStore) NewStore(d state.Deps) tool.StateStore { return k.newStoreFn(d) }

// itemStatus is a test helper that returns the status of the item with the
// given id in plan p, or a sentinel "<not found>" string to avoid silent misses.
func itemStatus(p plans.Plan, id string) plans.Status {
	it, ok := p.FindItem(id)
	if !ok {
		return plans.Status("<not found>")
	}
	return it.Status
}

func TestMarkStopped_StopsNonTerminalItemsOnly(t *testing.T) {
	s, ops, _ := newStore(t)
	ctx := context.Background()

	// Create a plan with one terminal item (done), one in-flight (in_progress),
	// and one not-yet-started (pending).
	_, err := s.Update(ctx, "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusDone},
		{ID: "b", Label: "B", Status: plans.StatusInProgress},
		{ID: "c", Label: "C", Status: plans.StatusPending},
	}})

	require.NoError(t, err)

	// Capture the operation ID opened for "b".
	got, _ := s.Get("main")
	bOpID := func() string {
		it, _ := got.FindItem("b")
		return it.OperationID
	}()
	require.NotEmpty(t, bOpID, "in_progress item must have an operation")

	changed, err := s.MarkStopped(ctx)
	require.NoError(t, err)
	require.Len(t, changed, 1, "one plan was changed")

	got, _ = s.Get("main")
	assert.Equal(t, plans.StatusDone, itemStatus(got, "a"), "terminal (done) item must not be touched")
	assert.Equal(t, plans.StatusStopped, itemStatus(got, "b"), "in_progress item must transition to stopped")
	assert.Equal(t, plans.StatusStopped, itemStatus(got, "c"), "pending item must transition to stopped")

	bOp, ok := ops.Get(bOpID)
	require.True(t, ok)
	assert.True(t, bOp.Closed, "the operation open for in_progress item must be closed")

	// Second call must be idempotent — already-stopped items are skipped.
	changed2, err := s.MarkStopped(ctx)
	require.NoError(t, err)
	assert.Empty(t, changed2, "idempotent: second call changes nothing")
}

func TestStoppedIsNotAgentSettable(t *testing.T) {
	assert.False(t, plans.StatusStopped.IsAgentSettable(), "agents cannot author stopped")
	assert.True(t, plans.StatusStopped.IsValid(), "stopped round-trips on replay")
}

func TestMarkStopped_ErrorItemSkipped(t *testing.T) {
	// Items already in a terminal error state must not be touched.
	s, _, _ := newStore(t)
	ctx := context.Background()

	_, err := s.Update(ctx, "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusError},
		{ID: "b", Label: "B", Status: plans.StatusPending},
	}})

	require.NoError(t, err)

	changed, err := s.MarkStopped(ctx)
	require.NoError(t, err)
	require.Len(t, changed, 1)

	got, _ := s.Get("main")
	assert.Equal(t, plans.StatusError, itemStatus(got, "a"), "error item must not be touched")
	assert.Equal(t, plans.StatusStopped, itemStatus(got, "b"), "pending item must be stopped")
}

func TestUpdate_RejectsStopped_IsNotAgentSettable(t *testing.T) {
	// An agent passing "stopped" in update_plan must get ErrInvalidStatus.
	s, _, _ := newStore(t)
	_, err := s.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusStopped},
	}})

	require.ErrorIs(t, err, plans.ErrInvalidStatus)
}

func TestInProgress_ReturnsOnlyPlansWithActiveItem(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()

	// Plan "main" has an in_progress item.
	_, err := s.Update(ctx, "main", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "a", Label: "A", Status: plans.StatusDone},
		{ID: "b", Label: "B", Status: plans.StatusInProgress},
	}})

	require.NoError(t, err)
	// Plan "side" is all pending — no active item.
	_, err = s.Update(ctx, "side", plans.ParentRef{}, plans.Content{Items: []plans.Item{
		{ID: "x", Label: "X", Status: plans.StatusPending},
	}})

	require.NoError(t, err)

	got := s.InProgress()
	require.Len(t, got, 1, "only the plan with an in_progress item")
	require.Equal(t, "main", got[0].Name)
}
