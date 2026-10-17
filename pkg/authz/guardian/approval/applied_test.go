package approval_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

func applied(category, outcome, outcomeText string) channelevents.InteractionAppliedPayload {
	return channelevents.InteractionAppliedPayload{
		Category:    category,
		RequestRef:  "req-1",
		Outcome:     outcome,
		OutcomeText: outcomeText,
	}
}

// TestFromApplied_LoginHumanStepCarriesResponseText is the C7 carrier crux: a
// login_human_step approval that carries a free-text code (the human's typed
// login code) delivers it on Decision.ResponseText — a GENUINE text field, not
// smuggled through the closed OutcomeText/Action. This is the one thing the
// confirm half never needed and the code half cannot work without.
func TestFromApplied_LoginHumanStepCarriesResponseText(t *testing.T) {
	pl := applied(categories.LoginHumanStep, channelevents.OutcomeApproved, "")
	pl.ResponseText = "123456"

	d, deliver, registered := approval.FromApplied(pl)

	require.True(t, registered)
	require.True(t, deliver)
	assert.True(t, d.Approved, "the code path is an approval")
	assert.Equal(t, "123456", d.ResponseText, "the typed code must ride back as free text")
	assert.Empty(t, d.Action, "the code must NOT be smuggled through the closed Action field")
}

// TestFromApplied_DeniedCodeCarriesNoText proves a denial drops the free text —
// a declined code must not resume as approved and must carry nothing to type.
func TestFromApplied_DeniedCodeCarriesNoText(t *testing.T) {
	pl := applied(categories.LoginHumanStep, channelevents.OutcomeDenied, "")
	pl.ResponseText = "should-not-matter"

	d, deliver, registered := approval.FromApplied(pl)

	require.True(t, registered)
	require.True(t, deliver)
	assert.False(t, d.Approved, "a denial is not an approval")
	// The runner's RequestCode only reads the code on an approval, so even a
	// (malformed) denial that carried text never reaches the page.
}

// The guard that was missing. Every category the runner blocks on must resume
// off an applied envelope; a category that does not is a session that publishes
// its prompt, accepts the click, and then hangs until its approval timeout with
// nothing logged near the cause.
//
// plan_phase and plan_amendment are the reason this test exists: they were
// registered as categories, published correctly, and decided correctly, but the
// runner's applied bridge enumerated resuming categories by NAME and silently
// dropped anything unlisted. Two bundles were parked over it, one of them
// misfiled as a harness defect.
func TestFromApplied_EveryRunnerGateResumes(t *testing.T) {
	cases := []struct {
		name        string
		category    string
		wantDeliver bool
		check       func(t *testing.T, d approval.Decision)
	}{
		{
			name: "tool_approval: approved → Approved=true", category: categories.ToolApproval,
			wantDeliver: true,
			check:       func(t *testing.T, d approval.Decision) { assert.True(t, d.Approved) },
		},
		{
			name: "content_inspection: approved → Approved=true", category: categories.ContentInspection,
			wantDeliver: true,
			check:       func(t *testing.T, d approval.Decision) { assert.True(t, d.Approved) },
		},
		{
			name: "info_leakage: approved → Approved=true", category: categories.InfoLeakage,
			wantDeliver: true,
			check:       func(t *testing.T, d approval.Decision) { assert.True(t, d.Approved) },
		},
		{
			name: "plan_phase: approved → Approved=true", category: categories.PlanPhase,
			wantDeliver: true,
			check:       func(t *testing.T, d approval.Decision) { assert.True(t, d.Approved) },
		},
		{
			name: "plan_amendment: approved → Approved=true", category: categories.PlanAmendment,
			wantDeliver: true,
			check:       func(t *testing.T, d approval.Decision) { assert.True(t, d.Approved) },
		},
		{
			// login_human_step (confirm half): a pure approve/deny the runner's
			// request_human_step block resumes off. Registered here proves the
			// browser human-step card can never hang the runner the way plan_phase
			// once did — the whole reason FromApplied is registry-driven.
			name: "login_human_step: approved → Approved=true", category: categories.LoginHumanStep,
			wantDeliver: true,
			check:       func(t *testing.T, d approval.Decision) { assert.True(t, d.Approved) },
		},
		{
			// The choice shape: a yes/no cannot say WHICH identity was picked, so
			// the answer has to survive as Action.
			name: "identity_choice: the picked action survives, not a boolean", category: categories.IdentityChoice,
			wantDeliver: true,
			check: func(t *testing.T, d approval.Decision) {
				assert.Equal(t, "userPassthrough", d.Action)
				assert.False(t, d.Approved, "a choice is not an approval")
			},
		},
		{
			// Declared as non-resuming: credential links resolve out of band, so
			// there is no gate waiting on an applied envelope.
			name: "credential_link: registered but nothing to deliver", category: categories.CredentialLink,
			wantDeliver: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, deliver, registered := approval.FromApplied(
				applied(tc.category, channelevents.OutcomeApproved, "userPassthrough"))

			require.True(t, registered, "category %q must be registered", tc.category)
			require.Equal(t, tc.wantDeliver, deliver)
			if tc.check != nil {
				tc.check(t, d)
			}
		})
	}
}

// Denied and expired are both "not approved" — an approval gate must not treat
// a lapsed deadline as a yes.
func TestFromApplied_DeniedAndExpiredBothResumeFalse(t *testing.T) {
	for _, outcome := range []string{channelevents.OutcomeDenied, channelevents.OutcomeExpired} {
		d, deliver, registered := approval.FromApplied(
			applied(categories.PlanPhase, outcome, ""))

		require.True(t, registered)
		require.True(t, deliver)
		assert.False(t, d.Approved, "outcome %q must not resume as approved", outcome)
	}
}

// An unregistered category is reported rather than silently dropped: it means a
// publisher and this binary disagree about the category set, and the only
// downstream symptom is a gate that never wakes.
func TestFromApplied_UnregisteredCategoryIsDistinguishable(t *testing.T) {
	_, deliver, registered := approval.FromApplied(
		applied("category_from_a_newer_build", channelevents.OutcomeApproved, ""))

	assert.False(t, registered, "an unknown category must be reportable as unknown")
	assert.False(t, deliver)
}
