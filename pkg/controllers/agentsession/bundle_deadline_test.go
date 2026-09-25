package agentsession

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestBundleProvisioningTimedOut pins the no-silent-hang backstop boundary AND
// its epoch: the deadline is measured from PROVISIONING START — the earliest
// not-ready bundle SpiceboxSession's CreationTimestamp — NOT the session's
// creation. The headline case is the bug this replaced: a session created long
// ago (e.g. a userPassthrough session whose user spent 12m linking credentials)
// whose bundle only started provisioning seconds ago must NOT be timed out. A
// zero provisioning-start (no not-ready bundle observed yet) never trips.
func TestBundleProvisioningTimedOut(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name              string
		provisioningStart time.Time
		want              bool
	}{
		{"zero provisioning start never times out", time.Time{}, false},
		{"provisioning started 10s ago (session may be far older) → not timed out", now.Add(-10 * time.Second), false},
		{"within deadline → not timed out", now.Add(-bundleReadyDeadline + time.Second), false},
		{"exactly at deadline → not timed out", now.Add(-bundleReadyDeadline), false},
		{"past deadline → timed out", now.Add(-bundleReadyDeadline - time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, bundleProvisioningTimedOut(tc.provisioningStart, now, bundleReadyDeadline))
		})
	}
}

// TestDetectorProvisioningTimedOut is the detector twin: the content-guard
// detector backstop is likewise measured from the earliest not-ready detector
// pod's CreationTimestamp, so a detector that started provisioning seconds ago
// on a much older (passthrough) session is not failed closed prematurely.
func TestDetectorProvisioningTimedOut(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name              string
		provisioningStart time.Time
		want              bool
	}{
		{"zero provisioning start never times out", time.Time{}, false},
		{"provisioning started 10s ago (session may be far older) → not timed out", now.Add(-10 * time.Second), false},
		{"exactly at deadline → not timed out", now.Add(-detectorReadyDeadline), false},
		{"past deadline → timed out", now.Add(-detectorReadyDeadline - time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, detectorProvisioningTimedOut(tc.provisioningStart, now))
		})
	}
}

// TestEarlierNonZero pins the running-min helper that tracks the earliest
// not-ready provisioning start across a reconcile loop: a zero is treated as
// "absent" so it never displaces a real timestamp, and a real candidate always
// replaces a zero accumulator.
func TestEarlierNonZero(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	earlier := base.Add(-time.Minute)
	cases := []struct {
		name      string
		cur       time.Time
		candidate time.Time
		want      time.Time
	}{
		{"zero accumulator adopts a real candidate", time.Time{}, base, base},
		{"zero candidate never displaces a real accumulator", base, time.Time{}, base},
		{"both zero stays zero", time.Time{}, time.Time{}, time.Time{}},
		{"earlier candidate wins", base, earlier, earlier},
		{"later candidate does not displace", earlier, base, earlier},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, earlierNonZero(tc.cur, tc.candidate))
		})
	}
}
