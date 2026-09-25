package setupui_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/setupui"
)

func threeStepTimeline() *setupui.Timeline {
	return setupui.NewTimeline([]string{"one", "two", "three"})
}

func TestNewTimeline_SeedsPendingConfiguring(t *testing.T) {
	tl := threeStepTimeline()
	snap := tl.Snapshot()

	assert.Equal(t, setupui.PhaseConfiguring, snap.Phase)
	require.Len(t, snap.Steps, 3)
	for _, st := range snap.Steps {
		assert.Equal(t, setupui.StatusPending, st.Status)
		assert.Zero(t, st.StartedAt)
		assert.Zero(t, st.EndedAt)
	}
	assert.Equal(t, []string{"one", "two", "three"}, []string{snap.Steps[0].Name, snap.Steps[1].Name, snap.Steps[2].Name})
}

func TestBegin_AdvancesAndCompletesPriorSteps(t *testing.T) {
	tl := threeStepTimeline()

	tl.Begin("two", 1000)
	snap := tl.Snapshot()

	require.Len(t, snap.Steps, 3)
	assert.Equal(t, setupui.StatusDone, snap.Steps[0].Status, "step before the one Begun should be marked done")
	assert.Equal(t, int64(1000), snap.Steps[0].StartedAt)
	assert.Equal(t, int64(1000), snap.Steps[0].EndedAt)

	assert.Equal(t, setupui.StatusActive, snap.Steps[1].Status)
	assert.Equal(t, int64(1000), snap.Steps[1].StartedAt)
	assert.Zero(t, snap.Steps[1].EndedAt)

	assert.Equal(t, setupui.StatusPending, snap.Steps[2].Status, "step after the one Begun stays pending")

	assert.Equal(t, setupui.PhaseRunning, snap.Phase)
}

func TestBegin_PreservesEarlierStepStartedAt(t *testing.T) {
	tl := threeStepTimeline()

	tl.Begin("one", 500)
	tl.Begin("two", 1500)
	snap := tl.Snapshot()

	assert.Equal(t, int64(500), snap.Steps[0].StartedAt, "step one's own StartedAt from its own Begin must survive")
	assert.Equal(t, int64(1500), snap.Steps[0].EndedAt, "step one's EndedAt is stamped when step two Begins")
	assert.Equal(t, setupui.StatusDone, snap.Steps[0].Status)
}

func TestBegin_Idempotent(t *testing.T) {
	tl := threeStepTimeline()

	tl.Begin("two", 1000)
	tl.Begin("two", 9999) // repeat call for the already-active step
	snap := tl.Snapshot()

	assert.Equal(t, setupui.StatusActive, snap.Steps[1].Status)
	assert.Equal(t, int64(1000), snap.Steps[1].StartedAt, "a repeat Begin on the active step must not reset StartedAt")
}

func TestBegin_UnknownStepIsNoOp(t *testing.T) {
	tl := threeStepTimeline()

	tl.Begin("does-not-exist", 1000)
	snap := tl.Snapshot()

	assert.Equal(t, setupui.PhaseConfiguring, snap.Phase)
	for _, st := range snap.Steps {
		assert.Equal(t, setupui.StatusPending, st.Status)
	}
}

func TestDetail_SetsStepDetail(t *testing.T) {
	tl := threeStepTimeline()
	tl.Begin("two", 1000)

	tl.Detail("two", "waiting for PostgreSQL")
	snap := tl.Snapshot()

	assert.Equal(t, "waiting for PostgreSQL", snap.Steps[1].Detail)
	assert.Empty(t, snap.Steps[0].Detail)
}

func TestDetail_UnknownStepIsNoOp(t *testing.T) {
	tl := threeStepTimeline()
	tl.Detail("does-not-exist", "whatever")
	// No panic, no visible change.
	snap := tl.Snapshot()
	for _, st := range snap.Steps {
		assert.Empty(t, st.Detail)
	}
}

func TestComplete_MarksAllStepsDoneAndReady(t *testing.T) {
	tl := threeStepTimeline()
	tl.Begin("two", 1000)

	tl.Complete(2000)
	snap := tl.Snapshot()

	assert.Equal(t, setupui.PhaseReady, snap.Phase)
	for _, st := range snap.Steps {
		assert.Equal(t, setupui.StatusDone, st.Status)
		assert.Equal(t, int64(2000), st.EndedAt)
		assert.NotZero(t, st.StartedAt)
	}
	// The step that had already Begun keeps its own StartedAt.
	assert.Equal(t, int64(1000), snap.Steps[1].StartedAt)
}

func TestFail_MarksStepFailedAndRecordsError(t *testing.T) {
	tl := threeStepTimeline()
	tl.Begin("two", 1000)

	tl.Fail("two", 1500, "boom")
	snap := tl.Snapshot()

	assert.Equal(t, setupui.PhaseFailed, snap.Phase)
	assert.Equal(t, setupui.StatusFailed, snap.Steps[1].Status)
	assert.Equal(t, "boom", snap.Steps[1].Error)
	assert.Equal(t, int64(1500), snap.Steps[1].EndedAt)
	// Earlier step is untouched by Fail (Begin already marked it done).
	assert.Equal(t, setupui.StatusDone, snap.Steps[0].Status)
	// Later step is untouched too (never started, never failed).
	assert.Equal(t, setupui.StatusPending, snap.Steps[2].Status)
}

func TestFail_UnknownStepStillFailsThePhase(t *testing.T) {
	tl := threeStepTimeline()

	// Simulates a validation failure before any step Begins — no step name
	// to attach the error to, but the overall phase must still flip.
	tl.Fail("", 100, "invalid config")
	snap := tl.Snapshot()

	assert.Equal(t, setupui.PhaseFailed, snap.Phase)
	for _, st := range snap.Steps {
		assert.Equal(t, setupui.StatusPending, st.Status)
		assert.Empty(t, st.Error)
	}
}

func TestComplete_DoesNotResurrectAFailedStep(t *testing.T) {
	tl := threeStepTimeline()
	tl.Begin("two", 1000)
	tl.Fail("two", 1500, "boom")

	tl.Complete(2000)
	snap := tl.Snapshot()

	assert.Equal(t, setupui.PhaseReady, snap.Phase, "Complete unconditionally sets the ready phase")
	assert.Equal(t, setupui.StatusFailed, snap.Steps[1].Status, "a failed step must not be overwritten by a later Complete")
	assert.Equal(t, "boom", snap.Steps[1].Error)
}

func TestNewTimeline_NeedsConfigDefaultsFalse(t *testing.T) {
	tl := threeStepTimeline()
	assert.False(t, tl.Snapshot().NeedsConfig)
}

func TestSetNeedsConfig_UpdatesSnapshotAndBroadcasts(t *testing.T) {
	tl := threeStepTimeline()
	ch, cancel := tl.Subscribe()
	defer cancel()

	tl.SetNeedsConfig(true)
	assert.True(t, tl.Snapshot().NeedsConfig)

	select {
	case snap := <-ch:
		assert.True(t, snap.NeedsConfig)
	default:
		t.Fatal("expected a snapshot to be pushed to the subscriber channel")
	}

	tl.SetNeedsConfig(false)
	assert.False(t, tl.Snapshot().NeedsConfig)
}

func TestSubscribe_ReceivesSnapshotOnMutationAndCleansUp(t *testing.T) {
	tl := threeStepTimeline()
	ch, cancel := tl.Subscribe()

	tl.Begin("one", 42)

	select {
	case snap := <-ch:
		assert.Equal(t, setupui.PhaseRunning, snap.Phase)
		assert.Equal(t, setupui.StatusActive, snap.Steps[0].Status)
	default:
		t.Fatal("expected a snapshot to be pushed to the subscriber channel")
	}

	cancel()
	tl.Begin("two", 43) // after cancel, nothing should arrive
	select {
	case snap := <-ch:
		t.Fatalf("unexpected snapshot after unsubscribe: %+v", snap)
	default:
	}
}

func TestSnapshot_IsACopyNotALiveView(t *testing.T) {
	tl := threeStepTimeline()
	snap := tl.Snapshot()

	tl.Begin("one", 1)

	assert.Equal(t, setupui.StatusPending, snap.Steps[0].Status, "a previously taken snapshot must not observe later mutations")
}

// TestConcurrentBeginAndSnapshot exercises the -race detector: parallel
// Begin/Detail/Snapshot/Subscribe calls must never race on Timeline's
// internal state.
func TestConcurrentBeginAndSnapshot(t *testing.T) {
	steps := []string{"a", "b", "c", "d", "e"}
	tl := setupui.NewTimeline(steps)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(4)
		go func(n int) {
			defer wg.Done()
			tl.Begin(steps[n%len(steps)], int64(n))
		}(i)
		go func() {
			defer wg.Done()
			_ = tl.Snapshot()
		}()
		go func(n int) {
			defer wg.Done()
			tl.SetNeedsConfig(n%2 == 0)
		}(i)
		go func(n int) {
			defer wg.Done()
			ch, cancel := tl.Subscribe()
			defer cancel()
			select {
			case <-ch:
			default:
			}
			_ = n
		}(i)
	}
	wg.Wait()
}
