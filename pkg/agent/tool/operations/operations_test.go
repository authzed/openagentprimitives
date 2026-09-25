package operations_test

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/stretchr/testify/require"
)

func TestBeginGetRoundTrip(t *testing.T) {
	r := operations.New(func() time.Time { return time.Unix(42, 0) }, nil)
	op := r.Begin("fetch the spicedb commit log")
	if !strings.HasPrefix(op.ID, "op-") {
		t.Fatalf("ID should start with op- prefix; got %q", op.ID)
	}
	if op.Description != "fetch the spicedb commit log" {
		t.Fatalf("Description = %q", op.Description)
	}
	if !op.CreatedAt.Equal(time.Unix(42, 0)) {
		t.Fatalf("CreatedAt = %v", op.CreatedAt)
	}

	got, ok := r.Get(op.ID)
	if !ok {
		t.Fatal("Get of just-Begun ID returned !ok")
	}
	if got.ID != op.ID {
		t.Fatalf("Get returned different ID: %q vs %q", got.ID, op.ID)
	}
}

func TestGetMissing(t *testing.T) {
	r := operations.New(nil, nil)
	if _, ok := r.Get("op-nope"); ok {
		t.Fatal("Get on missing ID should return ok=false")
	}
}

func TestRecordCallAppendsAndRejectsMissing(t *testing.T) {
	r := operations.New(func() time.Time { return time.Unix(100, 0) }, nil)
	op := r.Begin("test")
	idx0, ok := r.RecordCall(op.ID, tool.OperationCall{Tool: "code_gh", Reason: "fetch"})
	if !ok {
		t.Fatal("RecordCall on existing op should return ok=true")
	}
	if idx0 != 0 {
		t.Fatalf("first RecordCall index = %d, want 0", idx0)
	}
	idx1, ok := r.RecordCall(op.ID, tool.OperationCall{Tool: "code_git", Reason: "diff"})
	if !ok {
		t.Fatal("RecordCall(2) on existing op should return ok=true")
	}
	if idx1 != 1 {
		t.Fatalf("second RecordCall index = %d, want 1", idx1)
	}
	if idx, ok := r.RecordCall("op-bogus", tool.OperationCall{Tool: "x", Reason: "y"}); ok || idx != -1 {
		t.Fatalf("RecordCall on missing ID should return (-1, false); got (%d, %v)", idx, ok)
	}
	got, _ := r.Get(op.ID)
	if len(got.Calls) != 2 {
		t.Fatalf("len(Calls) = %d, want 2", len(got.Calls))
	}
	if got.Calls[0].Tool != "code_gh" || got.Calls[0].Reason != "fetch" {
		t.Errorf("Calls[0] = %+v", got.Calls[0])
	}
	if got.Calls[1].Tool != "code_git" {
		t.Errorf("Calls[1] = %+v", got.Calls[1])
	}
	if !got.Calls[0].At.Equal(time.Unix(100, 0)) {
		t.Errorf("At should default to now() when zero; got %v", got.Calls[0].At)
	}
	if !got.Calls[0].CompletedAt.IsZero() {
		t.Errorf("CompletedAt should default to zero (still in flight); got %v", got.Calls[0].CompletedAt)
	}
}

func TestRecordCall_ReturnsIncrementingIndices(t *testing.T) {
	r := operations.New(nil, nil)
	op := r.Begin("test")
	for want := 0; want < 5; want++ {
		idx, ok := r.RecordCall(op.ID, tool.OperationCall{Tool: "t", Reason: "r"})
		require.True(t, ok)
		require.Equal(t, want, idx)
	}
}

func TestCompleteCall_SetsCompletedAtOnRightCall(t *testing.T) {
	completedTime := time.Unix(200, 0)
	callCount := 0
	r := operations.New(func() time.Time {
		callCount++
		if callCount == 1 {
			return time.Unix(100, 0) // Begin
		}
		return completedTime
	}, nil)
	op := r.Begin("test")
	idx0, ok := r.RecordCall(op.ID, tool.OperationCall{Tool: "a", Reason: "r", At: time.Unix(101, 0)})
	require.True(t, ok)
	idx1, ok := r.RecordCall(op.ID, tool.OperationCall{Tool: "b", Reason: "r", At: time.Unix(102, 0)})
	require.True(t, ok)

	require.True(t, r.CompleteCall(op.ID, idx1))

	got, _ := r.Get(op.ID)
	require.True(t, got.Calls[idx0].CompletedAt.IsZero(), "call 0 must remain in flight")
	require.True(t, got.Calls[idx1].CompletedAt.Equal(completedTime))
}

func TestCompleteCall_UnknownIDReturnsFalse(t *testing.T) {
	r := operations.New(nil, nil)
	require.False(t, r.CompleteCall("op-nope", 0))
}

func TestCompleteCall_OutOfRangeIndexReturnsFalse(t *testing.T) {
	r := operations.New(nil, nil)
	op := r.Begin("test")
	idx, ok := r.RecordCall(op.ID, tool.OperationCall{Tool: "a", Reason: "r"})
	require.True(t, ok)

	require.False(t, r.CompleteCall(op.ID, idx+1), "index past the end of Calls")
	require.False(t, r.CompleteCall(op.ID, -1), "negative index")
}

func TestConcurrentAccess(t *testing.T) {
	r := operations.New(nil, nil)
	op := r.Begin("concurrent")
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, recorded := r.RecordCall(op.ID, tool.OperationCall{Tool: "x", Reason: "y"}); recorded {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 50 {
		t.Fatalf("ok=%d, want 50", ok.Load())
	}
	got, _ := r.Get(op.ID)
	if len(got.Calls) != 50 {
		t.Fatalf("len(Calls) = %d, want 50", len(got.Calls))
	}
}

func TestGetReturnsCloneCallsCannotMutateRegistry(t *testing.T) {
	r := operations.New(nil, nil)
	op := r.Begin("x")
	r.RecordCall(op.ID, tool.OperationCall{Tool: "a", Reason: "r"})
	got, _ := r.Get(op.ID)
	got.Calls[0].Tool = "MUTATED"
	got2, _ := r.Get(op.ID)
	if got2.Calls[0].Tool != "a" {
		t.Fatalf("registry mutated through returned slice; got %q", got2.Calls[0].Tool)
	}
}

func TestAllReturnsSnapshot(t *testing.T) {
	r := operations.New(nil, nil)
	r.Begin("a")
	r.Begin("b")
	all := r.All()
	if len(all) != 2 {
		t.Fatalf("All returned %d, want 2", len(all))
	}
}

func TestRegistry_SetParent_OperationParent(t *testing.T) {
	r := operations.New(nil, nil)
	parent := r.Begin("parent op")
	child := r.Begin("child op")

	ok := r.SetParent(child.ID, &tool.OperationParent{OperationID: parent.ID})
	require.True(t, ok)

	got, ok := r.Get(child.ID)
	require.True(t, ok)
	require.NotNil(t, got.Parent)
	require.Equal(t, parent.ID, got.Parent.OperationID)
	require.Nil(t, got.Parent.PlanItem)
}

func TestRegistry_SetParent_PlanItemRef(t *testing.T) {
	r := operations.New(nil, nil)
	op := r.Begin("anchored op")

	ok := r.SetParent(op.ID, &tool.OperationParent{
		PlanItem: &tool.PlanItemRef{Plan: "main", Item: "diff"},
	})
	require.True(t, ok)

	got, ok := r.Get(op.ID)
	require.True(t, ok)
	require.NotNil(t, got.Parent)
	require.Equal(t, "", got.Parent.OperationID)
	require.NotNil(t, got.Parent.PlanItem)
	require.Equal(t, "main", got.Parent.PlanItem.Plan)
	require.Equal(t, "diff", got.Parent.PlanItem.Item)
}

func TestRegistry_SetParent_UnknownIDReturnsFalse(t *testing.T) {
	r := operations.New(nil, nil)
	ok := r.SetParent("op-does-not-exist", &tool.OperationParent{OperationID: "x"})
	require.False(t, ok)
}

func TestRegistry_SetParent_BothFieldsPanics(t *testing.T) {
	r := operations.New(nil, nil)
	op := r.Begin("op")
	require.Panics(t, func() {
		_ = r.SetParent(op.ID, &tool.OperationParent{
			OperationID: "x",
			PlanItem:    &tool.PlanItemRef{Plan: "p", Item: "i"},
		})
	})
}

func TestRegistry_SetParent_NeitherFieldPanics(t *testing.T) {
	r := operations.New(nil, nil)
	op := r.Begin("op")
	require.Panics(t, func() {
		_ = r.SetParent(op.ID, &tool.OperationParent{})
	})
}

func TestRegistry_Close_MarksClosed(t *testing.T) {
	r := operations.New(nil, nil)
	op := r.Begin("op")

	require.True(t, r.Close(op.ID))

	got, ok := r.Get(op.ID)
	require.True(t, ok)
	require.True(t, got.Closed)
}

func TestRegistry_Close_UnknownIDReturnsFalse(t *testing.T) {
	r := operations.New(nil, nil)
	require.False(t, r.Close("op-nope"))
}

func TestRegistry_Close_IdempotentOnAlreadyClosed(t *testing.T) {
	r := operations.New(nil, nil)
	op := r.Begin("op")
	require.True(t, r.Close(op.ID))
	require.True(t, r.Close(op.ID)) // idempotent: still true

	got, _ := r.Get(op.ID)
	require.True(t, got.Closed)
}
