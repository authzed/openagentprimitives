package local

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func TestKind_Registered(t *testing.T) {
	k, ok := registry.Get("local")
	require.True(t, ok, "local kind must be registered via init()")
	assert.Equal(t, "local", k.Name())
}

func TestKind_RelayedByChannelsd_False(t *testing.T) {
	assert.False(t, (&Kind{}).RelayedByChannelsd(),
		"local kind is client-hosted, not relayed by channelsd")
}

func TestKind_SpawnsSessionOnInbound_False(t *testing.T) {
	assert.False(t, (&Kind{}).SpawnsSessionOnInbound(),
		"local kind owns exactly one TUI session; an inbound with no "+
			"active session must NOT spawn a phantom session")
}

func TestKind_TrivialMetadata(t *testing.T) {
	k := &Kind{}
	assert.Equal(t, "user", k.DefaultSessionScope())
	assert.True(t, k.UserAttributable(), "the local oap user is attributable")
	assert.Empty(t, k.RequiredSecretKeys(nil), "local needs no Secret keys")
	assert.Nil(t, k.SupportedMentionLookups(), "local supports no mention lookup")
	assert.Equal(t, "u-1", k.RenderMention("u-1"), "RenderMention returns the bare id")
	assert.Contains(t, k.Capabilities(), "text")
	assert.Contains(t, k.Capabilities(), "markdown")
	assert.Contains(t, k.Capabilities(), "plan")
}

func TestKind_LookupUser_Unsupported(t *testing.T) {
	_, _, err := (&Kind{}).LookupUser(nil, channelkinds.LookupDeps{},
		channelkinds.MentionLookupAny, "anyone")
	assert.ErrorIs(t, err, channelkinds.ErrMentionUnsupported)
}

func TestKind_SatisfiesInterface(t *testing.T) {
	var _ channelkinds.Kind = (*Kind)(nil)
}

func TestKind_ValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    spiceboxv1alpha1.ChannelSpec
		wantErr string
	}{
		{name: "bare local spec: valid", spec: spiceboxv1alpha1.ChannelSpec{Kind: "local"}},
		{
			name:    "local + slack block: rejected",
			spec:    spiceboxv1alpha1.ChannelSpec{Kind: "local", Slack: &spiceboxv1alpha1.SlackChannelConfig{}},
			wantErr: `spec.slack must be empty`,
		},
		{
			name:    "local + fake block: rejected",
			spec:    spiceboxv1alpha1.ChannelSpec{Kind: "local", Fake: &spiceboxv1alpha1.FakeChannelConfig{}},
			wantErr: `spec.fake must be empty`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Kind{}).ValidateSpec(&spiceboxv1alpha1.Channel{Spec: tc.spec})
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
