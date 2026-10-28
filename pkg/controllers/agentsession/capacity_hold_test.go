package agentsession

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
		{"hold shorter than base: held, base kept", 5 * time.Minute, short, true, 20 * time.Minute, 20 * time.Minute, true},
		{"disabled: base deadline, not held", -1, short, true, 8 * time.Minute, 8 * time.Minute, false},
		{"not stalled: base deadline, not held", 0, schedulingStall{}, false, 8 * time.Minute, 8 * time.Minute, false},
		{"non-capacity stall: base deadline, not held", 0, taint, true, 8 * time.Minute, 8 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{CapacityHoldTimeout: tc.timeout}
			dl, held := r.capacityHoldDeadline(tc.stall, tc.stalled, tc.base)
			assert.Equal(t, tc.wantDl, dl)
			assert.Equal(t, tc.wantHeld, held)
		})
	}
}
