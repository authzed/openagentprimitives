package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hasProjectStatus reports whether the effect list carries a ProjectStatus — the
// sentinel that the phase move is written to CR status (not merely audit-logged).
func hasProjectStatus(effs []Effect) bool {
	for _, e := range effs {
		if _, ok := e.(ProjectStatus); ok {
			return true
		}
	}
	return false
}

func TestArchiveSweepAndArchivedWake(t *testing.T) {
	t.Run("archive sweep from Idle: Phase=Succeeded AND Archived=true", func(t *testing.T) {
		got, effs := Transition(State{Phase: PhaseIdle, Region: RegionOperatorPost}.OrDefault(), ArchiveSweep{})
		assert.Equal(t, PhaseSucceeded, got.Phase, "swept Idle advances to Succeeded")
		assert.True(t, got.Archived, "the swept-to-Succeeded state must record that it is parked, not finished")
		assert.True(t, hasProjectStatus(effs), "archive sweep projects the new phase")
	})

	t.Run("stale archive sweep on Running: phase unchanged AND Archived stays false", func(t *testing.T) {
		got, _ := Transition(State{Phase: PhaseRunning, Region: RegionRunner}.OrDefault(), ArchiveSweep{})
		assert.Equal(t, PhaseRunning, got.Phase, "a stale sweep leaves the live phase alone")
		assert.False(t, got.Archived, "a sweep that does not park must not set the Archived bit")
	})

	t.Run("wake on archived Succeeded: resumes to Pending, Archived cleared, ProjectStatus emitted", func(t *testing.T) {
		got, effs := Transition(State{Phase: PhaseSucceeded, Archived: true}.OrDefault(), WakeRequested{})
		assert.Equal(t, PhasePending, got.Phase, "an archive-swept session is asleep — a wake resumes it")
		assert.False(t, got.Archived, "resuming clears the parked-by-sweep marker")
		assert.Equal(t, RegionOperatorPre, got.Region, "wake hands authority back to the operator-pre region")
		assert.False(t, got.Slept, "wake clears Slept so the lazy provisioner re-creates pods")
		assert.True(t, hasProjectStatus(effs), "resume must project Pending to status, not just append a log")
	})

	t.Run("wake on genuinely-completed Succeeded: stays sticky, AppendLog only", func(t *testing.T) {
		got, effs := Transition(State{Phase: PhaseSucceeded, Archived: false}.OrDefault(), WakeRequested{})
		assert.Equal(t, PhaseSucceeded, got.Phase, "a real completion stays terminal")
		assert.False(t, hasProjectStatus(effs), "no phase move, so nothing is projected")
		assert.Len(t, effs, 1, "a sticky terminal only audit-logs the event")
	})

	t.Run("wake on Failed even with Archived set: stays sticky, AppendLog only", func(t *testing.T) {
		got, effs := Transition(State{Phase: PhaseFailed, Archived: true}.OrDefault(), WakeRequested{})
		assert.Equal(t, PhaseFailed, got.Phase, "Failed is a genuine terminal — the archived exception is Succeeded-only")
		assert.False(t, hasProjectStatus(effs), "no phase move, so nothing is projected")
		assert.Len(t, effs, 1, "a sticky terminal only audit-logs the event")
	})
}

// Guard against a too-broad exception: a non-wake event on an archived Succeeded
// must still be sticky (only WakeRequested resumes).
func TestArchivedSucceededStaysStickyForNonWakeEvents(t *testing.T) {
	got, effs := Transition(State{Phase: PhaseSucceeded, Archived: true}.OrDefault(), Expired{})
	require.Equal(t, PhaseSucceeded, got.Phase, "Expired must not resume an archived session")
	assert.False(t, hasProjectStatus(effs), "no phase move on a non-wake event")
}
