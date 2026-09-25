package categories

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

func TestCredentialLinkRegistered(t *testing.T) {
	c, ok := channelinteractions.Get(CredentialLink)
	require.True(t, ok, "importing this package registers credential_link")
	assert.Equal(t, v1alpha1.AgentSessionPhaseAwaitingCredentials, c.Park)
	assert.Equal(t, channelinteractions.DecideRequester, c.Deciders)
	assert.Equal(t, channelinteractions.ResurfaceRegenerate, c.Resurface)
}

func TestCredentialUpdateRegistered(t *testing.T) {
	c, ok := channelinteractions.Get(CredentialUpdate)
	require.True(t, ok, "importing this package registers credential_update")

	assert.Equal(t, v1alpha1.AgentSessionPhaseAwaitingCredentials, c.Park,
		"credential_update reuses the existing AwaitingCredentials park rather than adding a phase")
	assert.Equal(t, channelinteractions.DecideRequester, c.Deciders)
	assert.Equal(t, channelinteractions.ResurfaceRegenerate, c.Resurface,
		"the signed link expires, so a re-surfaced card must be regenerated, not replayed from cache")
}

func TestIdentityChoiceRegistered(t *testing.T) {
	c, ok := channelinteractions.Get(IdentityChoice)
	require.True(t, ok, "importing this package registers identity_choice")
	assert.Equal(t, v1alpha1.AgentSessionPhaseAwaitingIdentityChoice, c.Park)
	assert.Equal(t, channelinteractions.DecideRequester, c.Deciders)
	assert.Equal(t, channelinteractions.ResurfaceCached, c.Resurface)
}

func TestWorkshopCredentialRegistered(t *testing.T) {
	c, ok := channelinteractions.Get(WorkshopCredential)
	require.True(t, ok, "importing this package registers workshop_credential")
	assert.Equal(t, "", c.Park, "workshop_credential must not park the builder session: the credential belongs to a different identity than the one running the builder")
	assert.Equal(t, channelinteractions.DecideRequester, c.Deciders)
	assert.Equal(t, channelinteractions.ResurfaceRegenerate, c.Resurface)
}

func TestPreconditionWaiverRegistered(t *testing.T) {
	c, ok := channelinteractions.Get(PreconditionWaiver)
	require.True(t, ok, "importing this package registers precondition_waiver")

	// Resume is the field whose omission makes the runner accept the click and
	// then hang its full approval timeout (category.go:119) — assert it, not just
	// that the row exists.
	assert.Equal(t, channelinteractions.ResumeApproval, c.Resume,
		"omit ResumeApproval and the runner drops the applied envelope and hangs")
	assert.Equal(t, v1alpha1.AgentSessionPhaseAwaitingDecision, c.Park)
	// DecideResourceOwners is load-bearing: the bound handler writes the slot
	// grant unconditionally, so this is the only thing gating an unauthorized
	// clicker — same policy as tool_approval and info_leakage.
	assert.Equal(t, channelinteractions.DecideResourceOwners, c.Deciders)
	assert.Equal(t, v1alpha1.AgentSessionConditionPreconditionWaiverPending, c.PendingCondition)
	assert.True(t, v1alpha1.IsApprovalCondition(c.PendingCondition),
		"the pending condition must be channelsd-owned or agentstatus.WriteOwned drops it")
	assert.Equal(t, channelinteractions.ToneCritical, c.Tone)
	assert.Equal(t, channelinteractions.ResurfaceCached, c.Resurface)
	require.NoError(t, c.Validate(), "the registered row must be well-formed")
}

// TestPreconditionWaiverSurvivesResetRegisterAll proves a test that Resets the
// registry can put precondition_waiver back via RegisterAll — the reason
// RegisterAll is exported and kept out of init proper.
func TestPreconditionWaiverSurvivesResetRegisterAll(t *testing.T) {
	channelinteractions.Reset()
	// Restore the process-wide registry for any later test in this package.
	t.Cleanup(func() {
		channelinteractions.Reset()
		RegisterAll()
	})

	_, ok := channelinteractions.Get(PreconditionWaiver)
	require.False(t, ok, "Reset must clear the registry (guards against a no-op test)")

	RegisterAll()
	c, ok := channelinteractions.Get(PreconditionWaiver)
	require.True(t, ok, "RegisterAll must re-register precondition_waiver")
	assert.Equal(t, channelinteractions.ResumeApproval, c.Resume)
}

// TestEveryPendingConditionIsWired closes the silent-drop class for EVERY
// current and future category: a Category.PendingCondition that is not in
// v1alpha1.ApprovalConditionTypes is dropped by agentstatus.WriteOwned (which
// gates ownership on IsApprovalCondition), and one that parks AwaitingDecision
// but is not in the pending-approval subset never derives phase=AwaitingDecision.
// Enumerating the live registry means a newly-registered category with an
// unwired condition fails HERE, not silently at runtime.
func TestEveryPendingConditionIsWired(t *testing.T) {
	for _, c := range channelinteractions.All() {
		if c.PendingCondition == "" {
			continue
		}
		assert.True(t, v1alpha1.IsApprovalCondition(c.PendingCondition),
			"%q: PendingCondition %q not in ApprovalConditionTypes → WriteOwned drops it", c.Name, c.PendingCondition)
		if c.Park == v1alpha1.AgentSessionPhaseAwaitingDecision {
			assert.True(t, v1alpha1.IsPendingApprovalCondition(c.PendingCondition),
				"%q parks AwaitingDecision but its condition %q isn't in the pending-approval subset", c.Name, c.PendingCondition)
		}
	}
}

func TestPortalAndPermissionRegistered(t *testing.T) {
	portal, ok := channelinteractions.Get(PortalAccess)
	require.True(t, ok, "portal_access must be registered")
	assert.Equal(t, "", portal.Park, "portal_access does not park")
	assert.Equal(t, channelinteractions.ResurfaceNone, portal.Resurface)

	perm, ok := channelinteractions.Get(PermissionRequest)
	require.True(t, ok, "permission_request must be registered")
	assert.Equal(t, "", perm.Park, "permission_request does not park the owner's session")
	assert.Equal(t, channelinteractions.DecideOwner, perm.Deciders)
	assert.Equal(t, channelinteractions.SurfaceDMOnly, perm.Surface)
}

func TestUserPreferenceConfirmRegistered(t *testing.T) {
	c, ok := channelinteractions.Get(UserPreferenceConfirm)
	require.True(t, ok, "importing this package registers user_preference_confirm")

	assert.Equal(t, channelinteractions.DecideRequester, c.Deciders,
		"answerable ONLY by the addressee — the turn author whose preference is being saved")
	assert.Equal(t, channelinteractions.ResumeApproval, c.Resume,
		"the runner blocks the preference commit on this decision")
	assert.Equal(t, v1alpha1.AgentSessionPhaseAwaitingDecision, c.Park)
	assert.Equal(t, v1alpha1.AgentSessionConditionPreferenceConfirmPending, c.PendingCondition)
	assert.True(t, v1alpha1.IsApprovalCondition(c.PendingCondition),
		"the pending condition must be channelsd-owned or agentstatus.WriteOwned drops it")
	assert.Equal(t, channelinteractions.ToneRoutine, c.Tone)
	assert.Equal(t, channelinteractions.ResurfaceCached, c.Resurface)
	require.NoError(t, c.Validate(), "the registered row must be well-formed")
}
