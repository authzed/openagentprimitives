package authz_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	cst "github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	exs "github.com/authzed/openagentprimitives/pkg/memory/kinds/extraction_state"
)

func TestWaitForExtraction_ReturnsImmediatelyOnComplete(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/n"}
	require.NoError(t, exs.Record(memory.WithSystemApproval(context.Background(), "test"), m, scope, exs.Content{
		TurnIndex: 3, Status: exs.StatusComplete, StartedAt: time.Now(), CompletedAt: time.Now(),
	}))
	start := time.Now()
	err := authz.WaitForExtraction(memory.WithSystemApproval(context.Background(), "test"), m, scope, 3, 2*time.Second)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 200*time.Millisecond, "should return immediately")
}

func TestWaitForExtraction_ReturnsOnDeadline(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/n"}
	start := time.Now()
	err := authz.WaitForExtraction(memory.WithSystemApproval(context.Background(), "test"), m, scope, 3, 200*time.Millisecond)
	require.NoError(t, err)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond)
	assert.Less(t, elapsed, 400*time.Millisecond)
}

func TestWaitForExtraction_PicksUpStateMidWait(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/n"}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = exs.Record(memory.WithSystemApproval(context.Background(), "test"), m, scope, exs.Content{
			TurnIndex: 3, Status: exs.StatusComplete, StartedAt: time.Now(), CompletedAt: time.Now(),
		})
	}()
	start := time.Now()
	err := authz.WaitForExtraction(memory.WithSystemApproval(context.Background(), "test"), m, scope, 3, 2*time.Second)
	require.NoError(t, err)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond)
	assert.Less(t, elapsed, 300*time.Millisecond)
}

func TestWaitForExtraction_ReturnsOnFailed(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/n"}
	require.NoError(t, exs.Record(memory.WithSystemApproval(context.Background(), "test"), m, scope, exs.Content{
		TurnIndex: 3, Status: exs.StatusFailed, StartedAt: time.Now(), Error: "llm error",
	}))
	start := time.Now()
	err := authz.WaitForExtraction(memory.WithSystemApproval(context.Background(), "test"), m, scope, 3, 2*time.Second)
	require.NoError(t, err, "failed status returns nil (caller falls through)")
	assert.Less(t, time.Since(start), 200*time.Millisecond)
}

func TestWaitForExtraction_NilMemoryNoOp(t *testing.T) {
	err := authz.WaitForExtraction(memory.WithSystemApproval(context.Background(), "test"), nil,
		memory.Scope{Kind: "session", ID: "ns/n"}, 3, 100*time.Millisecond)
	require.NoError(t, err)
}

// --- WaitForColdStartTask tests ---

func TestWaitForColdStartTask_ReturnsImmediatelyWhenPresent(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/cst1"}
	require.NoError(t, cst.Put(memory.WithSystemApproval(context.Background(), "test"), m, scope,
		cst.Content{Status: cst.StatusApprovedCleaned, CleanedText: "do less"}))

	start := time.Now()
	err := authz.WaitForColdStartTask(memory.WithSystemApproval(context.Background(), "test"), m, scope, 2*time.Second)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 200*time.Millisecond, "should return immediately when cold_start_task is present")
}

func TestWaitForColdStartTask_ReturnsOnDeadlineWhenAbsent(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/cst2"}

	start := time.Now()
	err := authz.WaitForColdStartTask(memory.WithSystemApproval(context.Background(), "test"), m, scope, 200*time.Millisecond)
	require.NoError(t, err, "deadline expiry must return nil (best-effort fail-open)")
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond)
	assert.Less(t, elapsed, 400*time.Millisecond)
}

func TestWaitForColdStartTask_PicksUpTaskMidWait(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/cst3"}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = cst.Put(memory.WithSystemApproval(context.Background(), "test"), m, scope, cst.Content{Status: cst.StatusRanWithoutScope})
	}()
	start := time.Now()
	err := authz.WaitForColdStartTask(memory.WithSystemApproval(context.Background(), "test"), m, scope, 2*time.Second)
	require.NoError(t, err)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond)
	assert.Less(t, elapsed, 300*time.Millisecond, "should unblock when cold_start_task appears")
}

func TestWaitForColdStartTask_NilMemoryNoOp(t *testing.T) {
	err := authz.WaitForColdStartTask(memory.WithSystemApproval(context.Background(), "test"), nil,
		memory.Scope{Kind: "session", ID: "ns/cst4"}, 100*time.Millisecond)
	require.NoError(t, err)
}
