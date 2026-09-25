package validator

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDecision_ZeroValueIsDeny(t *testing.T) {
	var d Decision
	assert.False(t, d.Allow, "zero Decision should not be Allow")
	assert.Nil(t, d.FailedOn, "zero Decision.FailedOn should be nil")
}

func TestCheckResult_Status_AcceptsKnownConstants(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{name: "pass status round-trips", status: StatusPass},
		{name: "fail status round-trips", status: StatusFail},
		{name: "skip status round-trips", status: StatusSkip},
		{name: "override-applied status round-trips", status: StatusOverrideApplied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := CheckResult{Status: tc.status}
			assert.Equal(t, tc.status, r.Status)
		})
	}
}
