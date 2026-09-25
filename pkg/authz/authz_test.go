package authz_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

func TestOutcomeConstants(t *testing.T) {
	assert.Equal(t, authz.Outcome(0), authz.OutcomeAllowed)
	assert.Equal(t, authz.Outcome(1), authz.OutcomeDenied)
}

func TestOutcomeString(t *testing.T) {
	assert.Equal(t, "Allowed", authz.OutcomeAllowed.String())
	assert.Equal(t, "Denied", authz.OutcomeDenied.String())
}

func TestResult_IsError(t *testing.T) {
	assert.False(t, authz.Result{Outcome: authz.OutcomeAllowed}.IsError(), "Allowed is not an error")
	assert.True(t, authz.Result{Outcome: authz.OutcomeDenied}.IsError(), "Denied is an error")
}

func TestErrNotYetMigrated(t *testing.T) {
	err := authz.ErrNotYetMigrated
	assert.True(t, errors.Is(err, authz.ErrNotYetMigrated))
	assert.Contains(t, err.Error(), "not yet migrated")
}

func TestSessionRefString(t *testing.T) {
	r := authz.SessionRef{Namespace: "ns", Name: "name"}
	assert.Equal(t, "ns/name", r.String())
}

func TestStateImpactConstants(t *testing.T) {
	assert.Equal(t, authz.StateImpact("stateless"), authz.Stateless)
	assert.Equal(t, authz.StateImpact("passthrough"), authz.Passthrough)
	assert.Equal(t, authz.StateImpact("readonly"), authz.Readonly)
	assert.Equal(t, authz.StateImpact("readwrite"), authz.Readwrite)
	assert.Equal(t, authz.StateImpact("external"), authz.External)
}

func TestStateImpact_CheckRequired(t *testing.T) {
	cases := []struct {
		name     string
		impact   authz.StateImpact
		expected bool
	}{
		{"Stateless: CheckRequired=false", authz.Stateless, false},
		{"Passthrough: CheckRequired=false", authz.Passthrough, false},
		{"Readonly: CheckRequired=true", authz.Readonly, true},
		{"Readwrite: CheckRequired=true", authz.Readwrite, true},
		{"External: CheckRequired=true", authz.External, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.impact.CheckRequired())
		})
	}
}

func TestEnforceModeConstants(t *testing.T) {
	assert.Equal(t, authz.EnforceMode("inherit"), authz.EnforceInherit)
	assert.Equal(t, authz.EnforceMode("always"), authz.EnforceAlways)
}
