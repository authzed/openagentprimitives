package memory_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestProbeLimit pins the one-extra-row contract. The probe is what makes
// "N rows with limit N" answerable: without it a full page is
// indistinguishable from a page that merely happened to fill.
func TestProbeLimit(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "a positive limit asks for exactly one extra row", limit: 100, want: 101},
		{name: "limit 1 asks for 2", limit: 1, want: 2},
		{name: "unlimited stays unlimited: nothing is discarded, so nothing needs probing", limit: 0, want: 0},
		{name: "a negative limit is unlimited too and is left alone", limit: -1, want: -1},
		{name: "MaxInt cannot be probed and must NOT wrap to a negative limit", limit: math.MaxInt, want: math.MaxInt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, memory.ProbeLimit(tc.limit))
		})
	}
}

// TestTrimProbe pins both directions: a set the limit cut short reports
// truncated, and a complete one does NOT. The false-positive direction is the
// one that matters — a notice printed over every complete listing is noise,
// and noise gets ignored.
func TestTrimProbe(t *testing.T) {
	rows := func(n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	cases := []struct {
		name      string
		n         int
		limit     int
		wantLen   int
		wantTrunc bool
	}{
		{name: "one row past the limit: probe row dropped, truncated", n: 4, limit: 3, wantLen: 3, wantTrunc: true},
		{name: "exactly at the limit: complete answer, NOT truncated", n: 3, limit: 3, wantLen: 3, wantTrunc: false},
		{name: "under the limit: complete answer, NOT truncated", n: 2, limit: 3, wantLen: 2, wantTrunc: false},
		{name: "empty: complete answer, NOT truncated", n: 0, limit: 3, wantLen: 0, wantTrunc: false},
		{name: "unlimited: never truncated, nothing trimmed", n: 5, limit: 0, wantLen: 5, wantTrunc: false},
		{name: "a store that ignored the probe cap: still trimmed to the limit, truncated", n: 10, limit: 3, wantLen: 3, wantTrunc: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated := memory.TrimProbe(rows(tc.n), tc.limit)
			assert.Len(t, got, tc.wantLen, "rows returned to the caller")
			assert.Equal(t, tc.wantTrunc, truncated, "truncation verdict")
		})
	}
}
