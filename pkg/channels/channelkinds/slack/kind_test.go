package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func TestRegistered(t *testing.T) {
	k, ok := registry.Get("slack")
	require.True(t, ok, "slack kind not registered")
	assert.Equal(t, "slack", k.Name(), "kind name")
	assert.Equal(t, "auto", k.DefaultSessionScope(), "default session scope")

	caps := k.Capabilities()
	require.GreaterOrEqual(t, len(caps), 2, "capabilities slice")
	assert.Equal(t, "text", caps[0], "capabilities[0]")
	assert.Equal(t, "markdown", caps[1], "capabilities[1]")
}

// TestKind_NewStreamDeltaSink_NonNilWithBotToken: the slack kind owns
// its StreamDeltaSink construction (was hardcoded in
// internal/cmd/channelsd/sender_resolver.go before this seam landed). With a
// valid bot-token Secret, the method returns a real sink wired to the
// slack web API; without a Secret it returns nil so the relay drops
// stream-delta envelopes silently.
func TestKind_NewStreamDeltaSink_NonNilWithBotToken(t *testing.T) {
	k := &Kind{}
	sink := k.NewStreamDeltaSink(channelkinds.Deps{
		Secret: &corev1.Secret{
			Data: map[string][]byte{SecretKeyBotToken: []byte("xoxb-test")},
		},
	})
	assert.NotNil(t, sink, "expected non-nil StreamDeltaSink with bot-token Secret")
}

func TestKind_NewStreamDeltaSink_NilWithoutSecret(t *testing.T) {
	k := &Kind{}
	sink := k.NewStreamDeltaSink(channelkinds.Deps{})
	assert.Nil(t, sink, "expected nil sink with no Secret")
}

func TestKind_Capabilities_Includes_Asset_MIMEs(t *testing.T) {
	caps := (&Kind{}).Capabilities()
	want := map[string]bool{
		"text":                  true,
		"markdown":              true,
		"asset:text/html":       true,
		"asset:image/png":       true,
		"asset:image/jpeg":      true,
		"asset:image/gif":       true,
		"asset:image/svg+xml":   true,
		"asset:text/css":        true,
		"asset:application/pdf": true,
	}
	for _, c := range caps {
		delete(want, c)
	}
	assert.Empty(t, want, "missing capabilities")
}

func TestKind_UserAttributable(t *testing.T) {
	assert.True(t, (&Kind{}).UserAttributable(),
		"slack.UserAttributable() must be true")
}

func TestKind_RelayedByChannelsd(t *testing.T) {
	assert.True(t, (&Kind{}).RelayedByChannelsd())
}

func TestKind_SpawnsSessionOnInbound(t *testing.T) {
	assert.True(t, (&Kind{}).SpawnsSessionOnInbound(),
		"slack is a durable channel; an inbound in a fresh thread spawns a new session")
}

func TestValidateSpec_MonitoringRole(t *testing.T) {
	k := &Kind{}
	cases := []struct {
		name    string
		ch      *spiceboxv1alpha1.Channel
		wantErr bool
	}{
		{"monitoring with channelId: ok", monitoringChannel("C_MON"), false},
		{"monitoring without outputDefaults: error", monitoringChannel(""), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := k.ValidateSpec(tc.ch)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRequiredSecretKeys_MonitoringRoleNeedsOnlyBotToken(t *testing.T) {
	k := &Kind{}
	monKeys := k.RequiredSecretKeys(monitoringChannel("C_MON"))
	assert.Equal(t, []string{SecretKeyBotToken}, monKeys, "monitoring channel needs only the bot token")

	// A non-monitoring slack channel still needs both keys — unchanged behavior.
	plain := &spiceboxv1alpha1.Channel{
		Spec: spiceboxv1alpha1.ChannelSpec{Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleBoth, Slack: &spiceboxv1alpha1.SlackChannelConfig{}},
	}
	assert.ElementsMatch(t, []string{SecretKeyBotToken, SecretKeyAppToken}, k.RequiredSecretKeys(plain))
}
