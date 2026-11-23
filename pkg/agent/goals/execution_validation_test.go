package goals

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExecutionValidationExplainsRejectedField(t *testing.T) {
	now := time.Now().UTC()
	valid := ExecutionTerms{ClassDigest: "pin", DueAt: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour), Bounds: ExecutionBounds{180, 10, 10000, 90}, Destination: PrivateDestination{Channel: "private", ChannelUID: "uid", Recipient: "owner"}, AllowedOperations: []string{"respond_to_user"}, Evidence: []string{"receipt"}}
	require.NoError(t, valid.validate(now, "owner"))
	for _, tc := range []struct {
		name, contains string
		change         func(*ExecutionTerms)
	}{
		{"elapsed start", "dueAt must be in the future", func(t *ExecutionTerms) { t.DueAt = now.Add(-time.Second) }},
		{"expiry", "expiresAt must be after dueAt", func(t *ExecutionTerms) { t.ExpiresAt = t.DueAt }},
		{"approval exceeds run", "bounds.approvalSeconds must be between 1 and bounds.durationSeconds=180", func(t *ExecutionTerms) { t.Bounds.ApprovalSeconds = 3600 }},
		{"zero turns", "bounds.turns", func(t *ExecutionTerms) { t.Bounds.Turns = 0 }},
		{"oversized tokens", "bounds.tokens", func(t *ExecutionTerms) { t.Bounds.Tokens = 10000001 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			terms := valid
			tc.change(&terms)
			err := terms.validate(now, "owner")
			require.ErrorIs(t, err, ErrInvalid)
			require.ErrorContains(t, err, tc.contains)
		})
	}
}
