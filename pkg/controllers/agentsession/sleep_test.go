// pkg/controllers/agentsession/sleep_test.go
//
// White-box unit test for the pure idle-sleep decision: an Idle channel
// session past its configured sleepAfter grace (and not already slept) is due
// to sleep. Pins the boundary (disabled, no lastIdleAt, already slept,
// within/past grace) without a client or wall-clock.
package agentsession

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestSandboxProvisioningDesired pins the lazy-provisioning gate: only an
// active/pending turn (Pending/Running) wants bundle SpiceboxSessions
// (re)created. Every parked phase (Idle, AwaitingRetry, AwaitingDecision,
// AwaitingCredentials) and terminal phase (Succeeded, Failed) stays false, so
// a reaped/parked session does not immediately bounce back to full pod count.
func TestSandboxProvisioningDesired(t *testing.T) {
	cases := []struct {
		phase string
		want  bool
	}{
		{spiceboxv1alpha1.AgentSessionPhasePending, true},
		{spiceboxv1alpha1.AgentSessionPhaseRunning, true},
		{spiceboxv1alpha1.AgentSessionPhaseIdle, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision, false},
		{spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, false},
		{spiceboxv1alpha1.AgentSessionPhaseSucceeded, false},
		{spiceboxv1alpha1.AgentSessionPhaseFailed, false},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			assert.Equal(t, tc.want, sandboxProvisioningDesired(tc.phase))
		})
	}
}

func TestIdleSleepDue(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *metav1.Time { t := metav1.NewTime(now.Add(d)); return &t }
	cases := []struct {
		name          string
		sleptAt       *metav1.Time
		lastIdleAt    *metav1.Time
		sleepAfter    time.Duration
		wantDue       bool
		wantRemaining time.Duration
	}{
		{"idle past grace: due", nil, at(-11 * time.Minute), 10 * time.Minute, true, 0},
		{"idle within grace: not due", nil, at(-3 * time.Minute), 10 * time.Minute, false, 7 * time.Minute},
		{"already slept: not due", at(-1 * time.Minute), at(-30 * time.Minute), 10 * time.Minute, false, 0},
		{"sleepAfter<=0 disabled: not due", nil, at(-1 * time.Hour), 0, false, 0},
		{"nil lastIdleAt: not due", nil, nil, 10 * time.Minute, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			due, remaining := idleSleepDue(tc.sleptAt, tc.lastIdleAt, tc.sleepAfter, now)
			assert.Equal(t, tc.wantDue, due)
			assert.Equal(t, tc.wantRemaining, remaining)
		})
	}
}
