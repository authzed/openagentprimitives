package testparallel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The envtest tiers boot an apiserver + etcd per PACKAGE, and e2e adds a
// SpiceDB container, so the concurrency that matters is per-package footprint
// rather than goroutines. These cases pin the shape of that trade-off.
func TestDerive(t *testing.T) {
	cases := []struct {
		name   string
		numCPU int
		want   int
	}{
		{
			// The configuration actually measured: on a 10-core box the e2e
			// tier ran 50 packages / 148 tests green at 8, in 201s against a
			// 465s baseline.
			name:   "10 cores: 8, the measured-green value",
			numCPU: 10, want: 8,
		},
		{
			// Never exceed what has been measured. A 64-core box does not make
			// a second etcd cheaper — the ceiling is about memory and container
			// pressure, not CPU.
			name:   "64 cores: capped at 8 rather than extrapolating past measurement",
			numCPU: 64, want: 8,
		},
		{
			name:   "4 cores: 2, leaving headroom for the OS and the test driver",
			numCPU: 4, want: 2,
		},
		{
			// Below the floor the tiers would serialize, which is the wedge
			// this whole change exists to remove.
			name:   "2 cores: floored at 2, never 0",
			numCPU: 2, want: 2,
		},
		{
			name:   "1 core: still 2",
			numCPU: 1, want: 2,
		},
		{
			// runtime.NumCPU cannot return this, but a caller passing a bogus
			// value must not produce -p=0, which go test rejects.
			name:   "nonsense input: floored, never zero or negative",
			numCPU: 0, want: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Derive(tc.numCPU))
		})
	}
}

// Whatever the machine, the result must be a value `go test -p` accepts.
func TestDerive_AlwaysUsable(t *testing.T) {
	for cpu := -4; cpu <= 128; cpu++ {
		got := Derive(cpu)
		assert.GreaterOrEqual(t, got, 2, "never serializes the tier")
		assert.LessOrEqual(t, got, 8, "never exceeds the measured ceiling")
	}
}
