package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// chWithDefaults builds a slack Channel carrying the supplied OutputDefaults.
// A nil od produces a Channel whose slack block has no OutputDefaults at all.
func chWithDefaults(t *testing.T, od *spiceboxv1alpha1.SlackOutputDefaults) *spiceboxv1alpha1.Channel {
	t.Helper()
	return &spiceboxv1alpha1.Channel{
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:  "slack",
			Role:  spiceboxv1alpha1.ChannelRoleOutput,
			Slack: &spiceboxv1alpha1.SlackChannelConfig{OutputDefaults: od},
		},
	}
}

func TestOutboundAnchor(t *testing.T) {
	cases := []struct {
		name         string
		ch           *spiceboxv1alpha1.Channel
		wantKey      string
		wantExternal map[string]string
		// wantErrField is the spec field the refusal must name. Empty means the
		// Channel is anchorable. The field name is asserted, not just the
		// error's presence: the Channel controller puts this string in the
		// Valid condition, and it is the only thing telling whoever applied the
		// Channel what to fill in.
		wantErrField string
	}{
		{
			name:         "new-thread-per-session: channel anchor only, relay adds thread_ts on first send",
			ch:           chWithDefaults(t, &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1", ThreadStrategy: "new-thread-per-session"}),
			wantKey:      "channel:C1",
			wantExternal: map[string]string{"channel_id": "C1"},
		},
		{
			name:         "empty threadStrategy defaults to channel anchor",
			ch:           chWithDefaults(t, &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1"}),
			wantKey:      "channel:C1",
			wantExternal: map[string]string{"channel_id": "C1"},
		},
		{
			name:         "direct: channel anchor, no thread",
			ch:           chWithDefaults(t, &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1", ThreadStrategy: "direct"}),
			wantKey:      "channel:C1",
			wantExternal: map[string]string{"channel_id": "C1"},
		},
		{
			name:         "static-thread: thread anchor seeded up front",
			ch:           chWithDefaults(t, &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1", ThreadStrategy: "static-thread", StaticThreadTS: "170.1"}),
			wantKey:      "thread:C1:170.1",
			wantExternal: map[string]string{"channel_id": "C1", "thread_ts": "170.1"},
		},
		{
			name:         "static-thread without staticThreadTs: refused naming staticThreadTs",
			ch:           chWithDefaults(t, &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1", ThreadStrategy: "static-thread"}),
			wantErrField: "spec.slack.outputDefaults.staticThreadTs",
		},
		{
			name:         "no channelId: refused naming channelId",
			ch:           chWithDefaults(t, &spiceboxv1alpha1.SlackOutputDefaults{}),
			wantErrField: "spec.slack.outputDefaults.channelId",
		},
		{
			name:         "no outputDefaults: refused naming channelId",
			ch:           chWithDefaults(t, nil),
			wantErrField: "spec.slack.outputDefaults.channelId",
		},
		{
			name:         "nil channel: refused naming channelId",
			ch:           nil,
			wantErrField: "spec.slack.outputDefaults.channelId",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, external, err := (&Kind{}).OutboundAnchor(tc.ch)
			if tc.wantErrField != "" {
				require.Error(t, err, "an unanchorable Channel must be refused")
				assert.Contains(t, err.Error(), tc.wantErrField,
					"the refusal must name the field a human has to fill in")
				assert.Empty(t, key)
				assert.Nil(t, external)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantKey, key)
			assert.Equal(t, tc.wantExternal, external)
		})
	}
}

// The anchor feeds spec.outputChannel, which must be byte-identical across
// re-derivations — a volatile value there would churn field ownership on
// every re-apply.
func TestOutboundAnchorIsPureFunctionOfChannel(t *testing.T) {
	ch := chWithDefaults(t, &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C1", ThreadStrategy: "static-thread", StaticThreadTS: "170.1"})
	k1, e1, err1 := (&Kind{}).OutboundAnchor(ch)
	k2, e2, err2 := (&Kind{}).OutboundAnchor(ch)
	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.Equal(t, k1, k2, "key must not vary between calls")
	assert.Equal(t, e1, e2, "external must not vary between calls")
}
