package agentstatus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFriendlyGate(t *testing.T) {
	cases := []struct {
		name                string
		reason, message     string
		wantTextContains    []string
		wantTextNotContains []string
		wantShortContains   string
	}{
		{
			name:             "BundlesProvisioning: progress tone, CRD-speak message stays off the user surface",
			reason:           "BundlesProvisioning",
			message:          "waiting for bundle SpiceboxSessions to become Ready",
			wantTextContains: []string{"Preparing"},
			wantTextNotContains: []string{
				"BundlesProvisioning", "SpiceboxSession", "Blocked", "Can't start",
			},
			wantShortContains: "Preparing",
		},
		{
			name:                "RunnerCreating: progress tone reads as starting, not blocked",
			reason:              "RunnerCreating",
			message:             "runner pod spawned",
			wantTextContains:    []string{"Starting the agent"},
			wantTextNotContains: []string{"RunnerCreating", "Can't start", "Blocked"},
			wantShortContains:   "Starting the agent",
		},
		{
			name:                "Unschedulable: problem tone, the scheduler's own message is the actionable part",
			reason:              "Unschedulable",
			message:             "0/1 nodes are available: 1 Insufficient memory.",
			wantTextContains:    []string{"Can't start yet", "capacity", "Insufficient memory"},
			wantTextNotContains: []string{"Unschedulable"},
			wantShortContains:   "Can't start",
		},
		{
			name:                "ModelMissingModel: settings blocker reads as a problem, no raw token",
			reason:              "ModelMissingModel",
			message:             "",
			wantTextContains:    []string{"Can't start yet", "model"},
			wantTextNotContains: []string{"ModelMissingModel"},
			wantShortContains:   "Can't start",
		},
		{
			name:    "unknown reason: humanized CamelCase fallback, message preserved, problem tone",
			reason:  "ImagePullBackOff",
			message: "pulling demo-toolchain:dev failed",
			wantTextContains: []string{
				"Can't start yet", "image pull back off", "pulling demo-toolchain:dev failed",
			},
			wantTextNotContains: []string{"ImagePullBackOff"},
			wantShortContains:   "Can't start",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, short := FriendlyGate(tc.reason, tc.message)
			require.NotEmpty(t, text)
			require.NotEmpty(t, short)
			for _, want := range tc.wantTextContains {
				assert.Contains(t, text, want)
			}
			for _, not := range tc.wantTextNotContains {
				assert.NotContains(t, text, not)
			}
			assert.Contains(t, short, tc.wantShortContains)
		})
	}
}

// Every mapped reason must render without leaking its machine token into
// either surface — the defect this file exists to prevent was a raw
// "BundlesProvisioning" shown to a user as "Blocked: BundlesProvisioning".
func TestFriendlyGate_NoRawReasonTokenEverLeaks(t *testing.T) {
	require.NotEmpty(t, gatePhrases, "the phrase map must exist and be populated")
	for reason := range gatePhrases {
		text, short := FriendlyGate(reason, "some detail")
		assert.NotContains(t, text, reason, "text leaks the machine token for %s", reason)
		assert.NotContains(t, short, reason, "short leaks the machine token for %s", reason)
		assert.NotEmpty(t, short, "short must never be empty for %s", reason)
	}
}

func TestFriendlyGateDetail(t *testing.T) {
	assert.Equal(t, "no model is configured: model demo-x missing",
		FriendlyGateDetail("ModelMissingModel", "model demo-x missing"))
	assert.Equal(t, "no model is configured",
		FriendlyGateDetail("ModelMissingModel", ""))
	// Unknown reasons humanize rather than leaking the CamelCase token.
	assert.Equal(t, "image pull back off: pull failed",
		FriendlyGateDetail("ImagePullBackOff", "pull failed"))
}
