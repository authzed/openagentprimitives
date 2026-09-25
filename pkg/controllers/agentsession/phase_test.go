package agentsession

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// derivePhase is now a thin projection reader over the folded lifecycle log:
// derivePhase(state) == lifecycle.Project(state).Phase. These tests pin the
// operator-pre/post transition sequences the operator drives to the phase the
// projection yields.

func TestDerivePhaseReadsProjection(t *testing.T) {
	// Folding [SettingsAccepted, RunnerClaimed] projects Phase=Running.
	st := lifecyclecore.Fold([]lifecyclecore.Event{lifecyclecore.SettingsAccepted{}, lifecyclecore.RunnerClaimed{}})
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, derivePhase(st))
	assert.Equal(t, lifecyclecore.Project(st).Phase, derivePhase(st),
		"derivePhase must be exactly the projection's phase")
}

func TestDerivePhase_FoldedSequences(t *testing.T) {
	cases := []struct {
		name   string
		events []lifecyclecore.Event
		want   string
	}{
		{
			name:   "empty log: Pending (the fold floor)",
			events: nil,
			want:   spiceboxv1alpha1.AgentSessionPhasePending,
		},
		{
			name:   "SettingsAccepted only: stays Pending",
			events: []lifecyclecore.Event{lifecyclecore.SettingsAccepted{}},
			want:   spiceboxv1alpha1.AgentSessionPhasePending,
		},
		{
			name:   "CredsMissing: AwaitingCredentials",
			events: []lifecyclecore.Event{lifecyclecore.SettingsAccepted{}, lifecyclecore.CredsMissing{}},
			want:   spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials,
		},
		{
			name:   "CredsMissing then CredsLinked: back to Pending",
			events: []lifecyclecore.Event{lifecyclecore.CredsMissing{}, lifecyclecore.CredsLinked{}},
			want:   spiceboxv1alpha1.AgentSessionPhasePending,
		},
		{
			name:   "RunnerClaimed: Running",
			events: []lifecyclecore.Event{lifecyclecore.SettingsAccepted{}, lifecyclecore.RunnerClaimed{}},
			want:   spiceboxv1alpha1.AgentSessionPhaseRunning,
		},
		{
			name:   "ProvablyUnschedulable: Failed",
			events: []lifecyclecore.Event{lifecyclecore.SettingsAccepted{}, lifecyclecore.ProvablyUnschedulable{}},
			want:   spiceboxv1alpha1.AgentSessionPhaseFailed,
		},
		{
			name:   "CredsTimeout: Failed",
			events: []lifecyclecore.Event{lifecyclecore.CredsMissing{}, lifecyclecore.CredsTimeout{}},
			want:   spiceboxv1alpha1.AgentSessionPhaseFailed,
		},
		{
			name:   "RunnerClaimed then RunnerCrash: Failed (backstop)",
			events: []lifecyclecore.Event{lifecyclecore.RunnerClaimed{}, lifecyclecore.RunnerCrash{}},
			want:   spiceboxv1alpha1.AgentSessionPhaseFailed,
		},
		{
			name:   "Unschedulable is surface-only: stays Pending",
			events: []lifecyclecore.Event{lifecyclecore.SettingsAccepted{}, lifecyclecore.Unschedulable{}},
			want:   spiceboxv1alpha1.AgentSessionPhasePending,
		},
		{
			name: "terminal is sticky: RunnerCrash then WakeRequested stays Failed",
			events: []lifecyclecore.Event{
				lifecyclecore.RunnerClaimed{}, lifecyclecore.RunnerCrash{}, lifecyclecore.WakeRequested{},
			},
			want: spiceboxv1alpha1.AgentSessionPhaseFailed,
		},
		{
			// Closes the T11 gap: with the runner emitting DecisionAsked into the
			// shared log, the operator's fold now projects AwaitingDecision for a
			// pending non-join decision (previously it demoted to Running).
			name: "DecisionAsked (tool_call): AwaitingDecision",
			events: []lifecyclecore.Event{
				lifecyclecore.SettingsAccepted{}, lifecyclecore.RunnerClaimed{},
				lifecyclecore.DecisionAsked{RequestID: "r1", Kind: lifecyclecore.DecisionToolCall},
			},
			want: string(lifecyclecore.PhaseAwaitingDecision),
		},
		{
			name: "DecisionAsked then DecisionResolved: back to Running",
			events: []lifecyclecore.Event{
				lifecyclecore.SettingsAccepted{}, lifecyclecore.RunnerClaimed{},
				lifecyclecore.DecisionAsked{RequestID: "r1", Kind: lifecyclecore.DecisionToolCall},
				lifecyclecore.DecisionResolved{RequestID: "r1", Approved: true},
			},
			want: spiceboxv1alpha1.AgentSessionPhaseRunning,
		},
		{
			// A timed-out leakage/content decision resolves deny-only and returns
			// the session to Running through the same fold path — no channelsd
			// timeout watcher involved.
			name: "DecisionAsked (leakage) then timeout-resolve: back to Running",
			events: []lifecyclecore.Event{
				lifecyclecore.SettingsAccepted{}, lifecyclecore.RunnerClaimed{},
				lifecyclecore.DecisionAsked{RequestID: "r1", Kind: lifecyclecore.DecisionLeakageShare},
				lifecyclecore.DecisionResolved{RequestID: "r1", Approved: false, TimedOut: true},
			},
			want: spiceboxv1alpha1.AgentSessionPhaseRunning,
		},
		{
			// join is visible via the pending set, not promoted to AwaitingDecision:
			// the phase stays where it was.
			name: "DecisionAsked (join): stays Running (not promoted)",
			events: []lifecyclecore.Event{
				lifecyclecore.SettingsAccepted{}, lifecyclecore.RunnerClaimed{},
				lifecyclecore.DecisionAsked{RequestID: "j1", Kind: lifecyclecore.DecisionJoin},
			},
			want: spiceboxv1alpha1.AgentSessionPhaseRunning,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, derivePhase(lifecyclecore.Fold(tc.events)))
		})
	}
}

// TestStampTerminalFinish pins the fold-to-terminal FinishedAt backfill: the
// steady-state phase write can fold the lifecycle log to a terminal phase
// (a runner-reported terminal event) without any explicit failure/success path
// having stamped FinishedAt, which left the reaper, CLI, admin UI, and session
// GC looking at a terminal session that never "finished". The backfill stamps
// `now` only on that gap — terminal phase, no existing stamp — and preserves an
// existing stamp and never touches a non-terminal phase.
func TestStampTerminalFinish(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	existing := metav1.NewTime(time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC))
	cases := []struct {
		name    string
		phase   string
		current *metav1.Time
		want    *metav1.Time
	}{
		{"Failed, no stamp: backfilled", spiceboxv1alpha1.AgentSessionPhaseFailed, nil, &now},
		{"Succeeded, no stamp: backfilled", spiceboxv1alpha1.AgentSessionPhaseSucceeded, nil, &now},
		{"Failed, already stamped: preserved", spiceboxv1alpha1.AgentSessionPhaseFailed, &existing, &existing},
		{"Succeeded, already stamped: preserved", spiceboxv1alpha1.AgentSessionPhaseSucceeded, &existing, &existing},
		{"Running, no stamp: untouched (nil)", spiceboxv1alpha1.AgentSessionPhaseRunning, nil, nil},
		{"Pending, no stamp: untouched (nil)", spiceboxv1alpha1.AgentSessionPhasePending, nil, nil},
		{"Idle, no stamp: untouched (nil)", spiceboxv1alpha1.AgentSessionPhaseIdle, nil, nil},
		{"Idle, spurious stamp: preserved (not our concern to clear)", spiceboxv1alpha1.AgentSessionPhaseIdle, &existing, &existing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stampTerminalFinish(tc.phase, tc.current, now))
		})
	}
}
