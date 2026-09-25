package wizardkeys

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateAuthzSubject is the gate on the one answer that can take a whole
// AgentClass down.
//
// A channel wizard's questions carry no validation any client evaluates
// (channelkinds.ValidateInputs refuses a Question.Validation outright), so
// this function is the ONLY thing between a typo and a Channel whose
// spec.authzSubject the apiserver's pattern — or, past that, the inbound
// pipeline's own authz.ValidateSubject — refuses. Each row is a shape a real
// answer takes.
func TestValidateAuthzSubject(t *testing.T) {
	cases := []struct {
		name      string
		subject   string
		errSubstr string
	}{
		{
			name:    "a service subject: accepted",
			subject: "service:demo-reviewbot-github",
		},
		{
			name:    "surrounding whitespace is trimmed before the check, not refused",
			subject: "  service:demo-reviewbot-github  ",
		},
		{
			name:      "empty: refused, because an empty subject is what this check exists to stop",
			subject:   "",
			errSubstr: "a subject is required",
		},
		{
			name:      "whitespace only: refused as empty rather than passed through as a subject",
			subject:   "   ",
			errSubstr: "a subject is required",
		},
		{
			name:      "a user subject: refused, and the message says what shape is wanted",
			subject:   "user:someone",
			errSubstr: `expected "service:<name>"`,
		},
		{
			name:      "no type prefix at all: refused",
			subject:   "demo-reviewbot",
			errSubstr: `expected "service:<name>"`,
		},
		{
			name:      "a service prefix with no id: refused",
			subject:   "service:",
			errSubstr: `expected "service:<name>"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAuthzSubject(tc.subject)
			if tc.errSubstr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSubstr)
			assert.Contains(t, err.Error(), KeyAuthzSubject,
				"the refusal must name the answer key, so an operator seeding it from a flag knows which one to fix")
		})
	}
}
