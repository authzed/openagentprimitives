package categories

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// TestLoginHumanStepRegistered pins the login_human_step row's security- and
// resume-relevant fields. Resume is the one whose omission makes the runner
// accept the click and then hang its full approval timeout (category.go's Resume
// doc), so it is asserted explicitly, not just that the row exists.
func TestLoginHumanStepRegistered(t *testing.T) {
	c, ok := channelinteractions.Get(LoginHumanStep)
	require.True(t, ok, "importing this package registers login_human_step")

	assert.Equal(t, channelinteractions.ResumeApproval, c.Resume,
		"omit ResumeApproval and the runner drops the applied envelope and hangs request_human_step")
	assert.Equal(t, v1alpha1.AgentSessionPhaseAwaitingDecision, c.Park)
	// DecideApprovers, not DecideResourceOwners: a sign-in step names no external
	// resource counterparty, and the confirm writes no grant, so the session
	// approve-set is the right standing gate (same as content_inspection).
	assert.Equal(t, channelinteractions.DecideApprovers, c.Deciders)
	assert.Equal(t, v1alpha1.AgentSessionConditionLoginHumanStepPending, c.PendingCondition)
	assert.True(t, v1alpha1.IsApprovalCondition(c.PendingCondition),
		"the pending condition must be channelsd-owned or agentstatus.WriteOwned drops it")
	assert.True(t, v1alpha1.IsPendingApprovalCondition(c.PendingCondition),
		"login_human_step parks AwaitingDecision, so its condition must be in the pending-approval subset or phase never derives AwaitingDecision")
	assert.Equal(t, channelinteractions.ToneRoutine, c.Tone)
	assert.Equal(t, channelinteractions.ResurfaceCached, c.Resurface)
	assert.False(t, c.Notice, "login_human_step is a prompt, not a notice")
	require.NoError(t, c.Validate(), "the registered row must be well-formed")
}

// TestLoginCaptchaRegistered pins the CAPTCHA notice: a zero-action notice
// (never a prompt), Unavailable tone (retrying the automation cannot help — a
// human must take over), and therefore requiring a NextStep at every publish
// site.
func TestLoginCaptchaRegistered(t *testing.T) {
	c, ok := channelinteractions.Get(LoginCaptcha)
	require.True(t, ok, "importing this package registers login_captcha")

	assert.True(t, c.Notice, "login_captcha is a NOTICE, not a prompt: there is no Done button to resume the agent")
	assert.Equal(t, channelinteractions.ToneUnavailable, c.Tone,
		"retrying the same automated flow cannot solve a CAPTCHA; Unavailable, not Degraded")
	assert.True(t, c.Tone.RequiresNextStep(),
		"the reader must be told to complete the step in the browser")
	assert.False(t, c.Terminal, "a human completing the CAPTCHA lets the session carry on")
	assert.Equal(t, channelinteractions.ResurfaceNone, c.Resurface)
	assert.Equal(t, "", c.Park, "a notice does not park the session")
	assert.Equal(t, channelinteractions.DeciderPolicy(""), c.Deciders, "a notice has no decision leg")
	require.NoError(t, c.Validate(), "the registered notice row must be well-formed")
}
