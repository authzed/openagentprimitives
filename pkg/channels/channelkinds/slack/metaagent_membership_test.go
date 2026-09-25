package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
)

// demoChannel is the minimal Channel a membership answer is computed against.
func demoChannel(t *testing.T) *spiceboxv1alpha1.Channel {
	t.Helper()
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: KindName, AgentClass: "demo-class"},
	}
}

// TestMetaagentMembership covers both arms of the answer the AgentSession
// reconciler copies onto Channel.status.
//
// Living on the kind that owns the id — rather than behind a `case "slack":`
// in a controller — is what makes both arms unit-testable from either
// configuration source, with no export shim to reach past the env read.
func TestMetaagentMembership(t *testing.T) {
	cases := []struct {
		name       string
		explicitID string // as wired by channelsd via SetMetaagentBotUserID
		envID      string // as read from the process environment (the operator)
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "no id from either source: False/MetaagentAppNotInstalled",
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ChannelReasonMetaagentAppNotInstalled,
		},
		{
			name:       "id from the environment only (the operator): True/Invited",
			envID:      "UMETAAGENTBOT",
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ChannelReasonMetaagentInvited,
		},
		{
			name:       "id wired explicitly only (channelsd): True/Invited",
			explicitID: "UMETAAGENTBOT",
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ChannelReasonMetaagentInvited,
		},
		{
			name:       "id from both sources: True/Invited",
			explicitID: "UWIREDBOT",
			envID:      "UENVBOT",
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ChannelReasonMetaagentInvited,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(clikit.EnvMetaagentSlackBotUserID, tc.envID)
			k := &Kind{}
			k.SetMetaagentBotUserID(tc.explicitID)

			m, ok := k.MetaagentMembership(demoChannel(t))
			require.True(t, ok, "slack always has an answer: it runs the metaagent as its own bot user")
			assert.Equal(t, tc.wantStatus, m.Status, "condition status")
			assert.Equal(t, tc.wantReason, m.Reason, "condition reason")
			assert.NotEmpty(t, m.Message, "message is where an operator reads the cause")
		})
	}
}

// TestMetaagentBotUserPrefersTheExplicitlyWiredID pins the precedence the
// environment fallback must not disturb: channelsd wires the id explicitly at
// start-up, and that wiring stays authoritative for the process that did it.
func TestMetaagentBotUserPrefersTheExplicitlyWiredID(t *testing.T) {
	t.Setenv(clikit.EnvMetaagentSlackBotUserID, "UENVBOT")

	k := &Kind{}
	assert.Equal(t, "UENVBOT", k.metaagentBotUser(),
		"with nothing wired, the process environment answers — this is the operator's path")

	k.SetMetaagentBotUserID("UWIREDBOT")
	assert.Equal(t, "UWIREDBOT", k.metaagentBotUser(),
		"an explicitly wired id must win over the environment")
}
