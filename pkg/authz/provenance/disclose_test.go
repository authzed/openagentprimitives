package provenance_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
)

// TestUnauthorizedIsTheSpecsWorkedExample runs the case from the design doc
// verbatim, because it is the one a reader will check this code against.
//
// A RAG document readable by tim, fred and sam; a Slack channel read by tim,
// fred and sarah. sarah is authorized on none of it, so answering on that
// channel is a leak — and the refusal must be able to say it was sarah.
func TestUnauthorizedIsTheSpecsWorkedExample(t *testing.T) {
	documentReaders := []string{"user:tim", "user:fred", "user:sam"}
	channelAudience := []string{"user:tim", "user:fred", "user:sarah"}

	assert.Equal(t, []string{"user:sarah"},
		provenance.Unauthorized(channelAudience, documentReaders),
		"the refusal must name who was not authorized; 'denied' alone cannot tell a misconfigured channel from a sensitive document")
	assert.False(t, provenance.MayDisclose(channelAudience, documentReaders))
}

// TestTheSameDataToASafeDestinationIsAllowed is the other half of that
// example: the tool-call destination everyone IS authorized on.
func TestTheSameDataToASafeDestinationIsAllowed(t *testing.T) {
	documentReaders := []string{"user:tim", "user:fred", "user:sam"}
	destinationAudience := []string{"user:tim", "user:fred"}

	assert.Empty(t, provenance.Unauthorized(destinationAudience, documentReaders))
	assert.True(t, provenance.MayDisclose(destinationAudience, documentReaders),
		"a strictly narrower destination is exactly the flow fine-grained provenance exists to permit")
}

func TestUnauthorizedEdges(t *testing.T) {
	cases := []struct {
		name     string
		audience []string
		readers  []string
		want     []string
	}{
		{
			name:     "an empty audience is vacuously safe: nobody to leak to",
			audience: nil,
			readers:  nil,
			want:     nil,
		},
		{
			name:     "no readers refuses every non-empty audience",
			audience: []string{"user:alice"},
			readers:  nil,
			want:     []string{"user:alice"},
		},
		{
			name:     "identical sets are safe",
			audience: []string{"user:alice", "user:bob"},
			readers:  []string{"user:bob", "user:alice"},
			want:     nil,
		},
		{
			name:     "extra readers are fine — the rule is subset, not equality",
			audience: []string{"user:alice"},
			readers:  []string{"user:alice", "user:bob", "user:carol"},
			want:     nil,
		},
		{
			name:     "every unauthorized member is reported, sorted",
			audience: []string{"user:zoe", "user:alice", "user:bob"},
			readers:  []string{"user:bob"},
			want:     []string{"user:alice", "user:zoe"},
		},
		{
			name:     "a duplicated unauthorized member is reported once",
			audience: []string{"user:zoe", "user:zoe"},
			readers:  nil,
			want:     []string{"user:zoe"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, provenance.Unauthorized(tc.audience, tc.readers))
		})
	}
}

// TestAnEmptyAudienceMustNotBeUsedForUnknown documents the one trap in this
// function, as an executable note.
//
// Empty means "nobody is present", which is safe. It does NOT mean "we could
// not work out who is present" — an unresolved audience passed here reads as
// permission. Callers resolving a destination must fall back to the coarse
// check on failure rather than handing an empty slice to this.
func TestAnEmptyAudienceMustNotBeUsedForUnknown(t *testing.T) {
	assert.True(t, provenance.MayDisclose(nil, nil),
		"documented behaviour: empty audience is safe, which is exactly why 'unknown' must never be spelled as empty")
}
