package channelevents

import "testing"
import "github.com/stretchr/testify/assert"

func TestInterruptKinds_ValidAndImplemented(t *testing.T) {
	for _, k := range []Kind{KindInterruptRequest, KindInterruptApplied} {
		assert.True(t, k.Valid(), "%s must be Valid", k)
		assert.True(t, k.Implemented(), "%s must be Implemented", k)
	}
}

func TestInterruptPayloads_RoundTrip(t *testing.T) {
	env, err := BuildEnvelope("ns", "s1", KindInterruptApplied, InterruptAppliedPayload{
		RequestID: "abc", Outcome: "rejected", Reason: "terraform_apply is changing state",
	})
	assert.NoError(t, err)
	assert.Equal(t, KindInterruptApplied, env.Kind)
}
