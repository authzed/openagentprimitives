package runner

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStopReasonAdvisoryExpected verifies that the expected stop_reasons
// produce no advisory — the regular loop path handles them.
func TestStopReasonAdvisoryExpected(t *testing.T) {
	for _, sr := range []string{"", "tool_use", "end_turn"} {
		t.Run("expected="+sr+": empty advisory", func(t *testing.T) {
			assert.Empty(t, stopReasonAdvisory(sr, 16384))
		})
	}
}

// TestStopReasonAdvisoryUnexpected verifies each non-expected reason
// produces an advisory that actually mentions the reason or a recognizable
// signal — so the model has something to act on.
func TestStopReasonAdvisoryUnexpected(t *testing.T) {
	cases := []struct {
		stopReason  string
		mustContain []string
	}{
		{"max_tokens", []string{"TRUNCATED", "max_tokens=16384", "split"}},
		{"stop_sequence", []string{"stop_sequence"}},
		{"pause_turn", []string{"pause_turn"}},
		{"refusal", []string{"refusal", "agent_work_complete"}},
		{"some_future_reason", []string{"unexpected stop_reason", "some_future_reason"}},
	}
	for _, tc := range cases {
		t.Run(tc.stopReason+": non-empty advisory with [runner-warning] prefix + signal substrings", func(t *testing.T) {
			got := stopReasonAdvisory(tc.stopReason, 16384)
			require.NotEmpty(t, got, "advisory must not be empty for unexpected stop_reason")
			assert.True(t, strings.HasPrefix(got, "[runner-warning]"),
				"missing [runner-warning] prefix: %q", got)
			for _, sub := range tc.mustContain {
				assert.Contains(t, got, sub, "advisory missing substring")
			}
		})
	}
}

// TestUserFacingStopReason is the RC-3 regression: the message the USER sees when
// a turn ends on a non-tool_use stop_reason (e.g. a provider `refusal`) must be a
// clean, real explanation — NEVER the agent-facing "[runner-warning] …" advisory
// (which now goes to operator logs + a runner note instead).
func TestUserFacingStopReason(t *testing.T) {
	// Substrings that would betray internal/agent-facing detail leaking to the user.
	leaks := []string{"[runner-warning]", "agent_work_complete", "respond_to_user", "stop_reason", "tool_use", "max_tokens="}
	for _, sr := range []string{"refusal", "max_tokens", "stop_sequence", "pause_turn", "some_future_reason"} {
		t.Run(sr+": clean user message, no internal leak", func(t *testing.T) {
			msg := userFacingStopReason(sr)
			require.NotEmpty(t, msg, "user-facing message must not be empty")
			for _, bad := range leaks {
				assert.NotContains(t, msg, bad, "user message must not leak internal detail %q", bad)
			}
		})
	}
	assert.Contains(t, userFacingStopReason("refusal"), "rephras", "a refusal should invite the user to rephrase")
}
