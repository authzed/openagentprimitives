package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApprovalButtonValue_RoundTrip(t *testing.T) {
	for _, disc := range []string{discShowSettings, discInteractionDetails} {
		t.Run(disc, func(t *testing.T) {
			enc := encodeApprovalButtonValue(disc, "req-1", "approve", "ns/name")
			got, ok := decodeApprovalButtonValue(enc)
			require.True(t, ok)
			assert.Equal(t, disc, got.V)
			assert.Equal(t, "req-1", got.R)
			assert.Equal(t, "approve", got.D)
			assert.Equal(t, "ns/name", got.S)
		})
	}
}

func TestApprovalButtonValue_RejectsForeignDiscriminator(t *testing.T) {
	_, ok := decodeApprovalButtonValue(`{"v":"something_else","r":"x"}`)
	assert.False(t, ok)
}

// TestApprovalButtonValue_RejectsRetiredDiscriminator verifies that retired
// discriminators do not decode: a stale in-flight button carrying one is
// treated as foreign, not silently accepted.
func TestApprovalButtonValue_RejectsRetiredDiscriminator(t *testing.T) {
	for _, disc := range []string{"slice2_approval", "info_leakage_approval", "slice2_show_details", "queued_messages"} {
		_, ok := decodeApprovalButtonValue(`{"v":"` + disc + `","r":"x"}`)
		assert.False(t, ok, "retired discriminator %q must not decode", disc)
	}
}

// TestInteractionButtonValue_RoundTrip verifies encodeInteractionButtonValue
// produces a discInteraction blob that decodes with Category carried in the
// "c" field alongside the shared r/d/s fields.
func TestInteractionButtonValue_RoundTrip(t *testing.T) {
	enc := encodeInteractionButtonValue("req-1", "agent", "identity_choice", "ns/name")
	got, ok := decodeApprovalButtonValue(enc)
	require.True(t, ok)
	assert.Equal(t, discInteraction, got.V)
	assert.Equal(t, "req-1", got.R, "requestRef")
	assert.Equal(t, "agent", got.D, "actionId")
	assert.Equal(t, "identity_choice", got.C, "category")
	assert.Equal(t, "ns/name", got.S, "sessRef")
}

// TestApprovalButtonValue_NonInteraction_OmitCategory verifies the C field
// doesn't leak into the wire encoding of a non-interaction discriminator (they
// never set it, and "omitempty" keeps their JSON shape unchanged for any
// in-flight message decoded across a rollout).
func TestApprovalButtonValue_NonInteraction_OmitCategory(t *testing.T) {
	enc := encodeApprovalButtonValue(discShowSettings, "req-1", "approve", "ns/name")
	assert.NotContains(t, enc, `"c"`, "non-interaction discriminators must not emit the c field")
}
