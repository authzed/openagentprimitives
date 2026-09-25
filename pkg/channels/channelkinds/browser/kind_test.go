package browser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func TestKind_Registered(t *testing.T) {
	k, ok := registry.Get(KindName)
	require.True(t, ok, "browser kind must be registered via init()")
	assert.Equal(t, KindName, k.Name())
}

// TestKindNameIsTheWireValue pins the STRING, deliberately, where every other
// test in the tree now writes browser.KindName. "browser" is persisted: it is
// the value stored in Channel.spec.kind (whose CRD enum admits it), in
// AgentSession.spec.inputChannel.kind, and in the channel-kind label a
// session is selected by. Changing the const would silently re-spell all
// three on newly-created objects while every symbol-based test stayed green.
// This literal is the one place that is not allowed to follow the const.
func TestKindNameIsTheWireValue(t *testing.T) {
	assert.Equal(t, "browser", KindName,
		"Channel.spec.kind is a persisted wire value; changing it needs a CRD enum change and a migration story, not a const edit")
	assert.Equal(t, "browser:demo-ns/demo-chan", ChannelKey("demo-ns", "demo-chan"),
		"the channel key's prefix is hashed into the channel-key label the inbound pipeline correlates on")
}

func TestKind_RelayedByChannelsd_False(t *testing.T) {
	assert.False(t, (&Kind{}).RelayedByChannelsd(),
		"browser kind is client-hosted, not relayed by channelsd")
}

func TestKind_SpawnsSessionOnInbound_False(t *testing.T) {
	assert.False(t, (&Kind{}).SpawnsSessionOnInbound(),
		"browser kind owns exactly one session for the lifetime of the browser "+
			"session its host pre-created; an inbound with no active session must "+
			"NOT spawn a phantom session")
}

func TestKind_TrivialMetadata(t *testing.T) {
	k := &Kind{}
	assert.Equal(t, "user", k.DefaultSessionScope())
	assert.True(t, k.UserAttributable(), "the browser session's viewer is attributable")
	assert.Empty(t, k.RequiredSecretKeys(nil), "browser needs no Secret keys")
	assert.Nil(t, k.SupportedMentionLookups(), "browser supports no mention lookup")
	assert.Equal(t, "u-1", k.RenderMention("u-1"), "RenderMention returns the bare id")
	assert.Contains(t, k.Capabilities(), "text")
	assert.Contains(t, k.Capabilities(), "markdown")
	assert.Contains(t, k.Capabilities(), "plan")
	// The browser page can attach an HTML artifact (delivered as a download
	// chip), so it advertises the asset:text/html attach capability — this is
	// what enables respond_to_user's `attached` field on browser sessions.
	assert.Contains(t, k.Capabilities(), "asset:text/html")
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
		{name: "bare browser spec: valid", spec: spiceboxv1alpha1.ChannelSpec{Kind: KindName}},
		{
			name:    "browser + slack block: rejected",
			spec:    spiceboxv1alpha1.ChannelSpec{Kind: KindName, Slack: &spiceboxv1alpha1.SlackChannelConfig{}},
			wantErr: `spec.slack must be empty`,
		},
		{
			name:    "browser + fake block: rejected",
			spec:    spiceboxv1alpha1.ChannelSpec{Kind: KindName, Fake: &spiceboxv1alpha1.FakeChannelConfig{}},
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
