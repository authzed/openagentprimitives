package agentsession

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestCapacityShortfall(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"insufficient cpu: shortfall", "0/3 nodes are available: 3 Insufficient cpu.", true},
		{"insufficient memory among other reasons: shortfall",
			"0/6 nodes are available: 1 node(s) didn't match PersistentVolume's node affinity, 5 Insufficient memory.", true},
		{"pod count limit: shortfall", "0/2 nodes are available: 2 Too many pods.", true},
		{"taint only: not a shortfall", "0/6 nodes are available: 6 node(s) had untolerated taint(s).", false},
		{"volume affinity only: not a shortfall", "0/6 nodes are available: 6 node(s) didn't match PersistentVolume's node affinity.", false},
		{"empty: not a shortfall", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, capacityShortfall(tc.msg))
		})
	}
}

func TestCapacityHoldDeadline(t *testing.T) {
	short := schedulingStall{reason: "0/3 nodes are available: 3 Insufficient cpu."}
	taint := schedulingStall{reason: "0/3 nodes are available: 3 node(s) had untolerated taint(s)."}
	cases := []struct {
		name     string
		timeout  time.Duration
		stall    schedulingStall
		stalled  bool
		base     time.Duration
		wantDl   time.Duration
		wantHeld bool
	}{
		{"default timeout, capacity stall: held for 1h", 0, short, true, 8 * time.Minute, time.Hour, true},
		{"configured timeout, capacity stall: held for it", 30 * time.Minute, short, true, 8 * time.Minute, 30 * time.Minute, true},
		{"hold no longer than base: not held, base kept", 5 * time.Minute, short, true, 20 * time.Minute, 20 * time.Minute, false},
		{"disabled: base deadline, not held", -1, short, true, 8 * time.Minute, 8 * time.Minute, false},
		{"not stalled: base deadline, not held", 0, schedulingStall{}, false, 8 * time.Minute, 8 * time.Minute, false},
		{"non-capacity stall: base deadline, not held", 0, taint, true, 8 * time.Minute, 8 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{CapacityHoldTimeout: tc.timeout}
			sess := &spiceboxv1alpha1.AgentSession{}
			sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
			dl, held := r.capacityHoldDeadline(sess, tc.stall, tc.stalled, tc.base)
			assert.Equal(t, tc.wantDl, dl)
			assert.Equal(t, tc.wantHeld, held)
		})
	}
}

// Only a booting session is held; a Running or terminal one keeps the base.
func TestCapacityHoldDeadline_OnlyWhileBooting(t *testing.T) {
	short := schedulingStall{reason: "0/3 nodes are available: 3 Insufficient cpu."}
	cases := []struct {
		phase    string
		wantHeld bool
	}{
		{"", true},
		{spiceboxv1alpha1.AgentSessionPhasePending, true},
		{spiceboxv1alpha1.AgentSessionPhaseRunning, false},
		{spiceboxv1alpha1.AgentSessionPhaseFailed, false},
		{spiceboxv1alpha1.AgentSessionPhaseIdle, false},
	}
	for _, tc := range cases {
		t.Run("phase "+tc.phase, func(t *testing.T) {
			sess := &spiceboxv1alpha1.AgentSession{}
			sess.Status.Phase = tc.phase
			_, held := (&Reconciler{}).capacityHoldDeadline(sess, short, true, 8*time.Minute)
			assert.Equal(t, tc.wantHeld, held)
		})
	}
}
