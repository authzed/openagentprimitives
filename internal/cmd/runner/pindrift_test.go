package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPinDriftAction(t *testing.T) {
	cases := []struct {
		name         string
		mode         string
		drifted      bool
		wantWithhold bool
		wantEscalate bool
		wantWarn     bool
	}{
		// Not drifted: all modes produce no-op.
		{name: "not drifted / no rule", mode: "", drifted: false},
		{name: "not drifted / off", mode: "off", drifted: false},
		{name: "not drifted / block", mode: "block", drifted: false},
		{name: "not drifted / approve", mode: "approve", drifted: false},
		{name: "not drifted / warn", mode: "warn", drifted: false},

		// Drifted with no rule: observe only (no action).
		{name: "drifted / no rule (observe only)", mode: "", drifted: true},
		{name: "drifted / off (observe only)", mode: "off", drifted: true},

		// Drifted with enforcement modes.
		{name: "drifted / block: withhold", mode: "block", drifted: true,
			wantWithhold: true},
		{name: "drifted / approve: escalate + warn", mode: "approve", drifted: true,
			wantEscalate: true, wantWarn: true},
		{name: "drifted / warn: warn only", mode: "warn", drifted: true,
			wantWarn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withhold, escalate, warn := pinDriftAction(tc.mode, tc.drifted)
			assert.Equal(t, tc.wantWithhold, withhold, "withhold")
			assert.Equal(t, tc.wantEscalate, escalate, "escalate")
			assert.Equal(t, tc.wantWarn, warn, "warn")
		})
	}
}
