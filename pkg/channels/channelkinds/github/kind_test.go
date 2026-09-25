package github

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func TestKind_InterfaceAnswers(t *testing.T) {
	k := Kind{}
	assert.Equal(t, "github", k.Name())
	assert.Equal(t, "thread", k.DefaultSessionScope(), "one session per PR")
	assert.Nil(t, k.Capabilities(), "input-only: no rendering surface")
	assert.False(t, k.RelayedByChannelsd(), "channelsd hosts no transport for this kind; webd receives")
	assert.True(t, k.SpawnsSessionOnInbound(), "a never-reviewed PR legitimately starts a session")
	assert.False(t, k.UserAttributable(), "the PR author has no AP identity and consented to nothing")
	assert.False(t, k.AllowsSyntheticIdentity())
	assert.False(t, k.SupportsMonitoring())
	assert.Nil(t, k.NewMonitoringSender(channelkinds.Deps{}))
	assert.Nil(t, k.WebAuthenticator(channelkinds.WebAuthDeps{}))
	assert.NotNil(t, k.WebhookReceiver(channelkinds.Deps{}), "this kind IS webhook-routable")
}

func TestNewSender_RefusesToSend(t *testing.T) {
	_, err := Kind{}.NewSender(channelkinds.Deps{}).Send(
		context.Background(), channelkinds.SessionInfo{}, channelevents.Envelope{})
	require.Error(t, err, "input-only: replies go to the paired Slack Channel")
}

// TestKind_Wizard_ReturnsTheRealWizardNotUnavailable is what would have
// caught the gap this kind shipped with: Wizard() returning
// channelkinds.UnavailableWizard because no task in the reviewbot plan wired
// the real wizard type into it. A nil check can't catch that —
// UnavailableWizard is also non-nil — so this asserts an identity and a
// behavior a placeholder cannot fake: the concrete type, and a Handoff() that
// actually describes the App-manifest round trip rather than
// UnavailableWizard's constant (nil, error).
//
// Handoff and not Inputs, because Handoff needs no cluster: Inputs lists the
// namespace's AgentClasses and refuses a namespace with none, which is
// indistinguishable here from a placeholder refusing everything.
func TestKind_Wizard_ReturnsTheRealWizardNotUnavailable(t *testing.T) {
	w, ok := Kind{}.Wizard().(*wizard)
	require.True(t, ok, "Kind.Wizard() must return the real *wizard type, not channelkinds.UnavailableWizard")

	spec, err := w.Handoff(context.Background(), channelkinds.WizardInput{Namespace: "default"})
	require.NoError(t, err, "the real wizard's Handoff must succeed given a namespace; "+
		"UnavailableWizard.Handoff always returns a non-nil error")
	require.NotNil(t, spec, "this kind's whole flow is a browser round trip; a nil spec is a placeholder")

	names := make([]string, 0, len(spec.FallbackInputs))
	for _, q := range spec.FallbackInputs {
		names = append(names, q.Name)
	}
	assert.Equal(t, []string{"app-id", "slug", "private-key-path", "webhook-secret", "installation-id"}, names,
		"the manual fallback, in order — a placeholder wizard declares no questions at all")
}

func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    spiceboxv1alpha1.ChannelSpec
		wantErr string
	}{
		{name: "role=output rejected",
			spec: spiceboxv1alpha1.ChannelSpec{Kind: "github", Role: spiceboxv1alpha1.ChannelRoleOutput,
				GitHub: &spiceboxv1alpha1.GitHubChannelConfig{AppSlug: "demo-reviewbot"}},
			wantErr: "must declare role"},
		{name: "missing github block rejected",
			spec:    spiceboxv1alpha1.ChannelSpec{Kind: "github", Role: spiceboxv1alpha1.ChannelRoleInput},
			wantErr: "spec.github is required"},
		{name: "sibling slack block rejected",
			spec: spiceboxv1alpha1.ChannelSpec{Kind: "github", Role: spiceboxv1alpha1.ChannelRoleInput,
				GitHub: &spiceboxv1alpha1.GitHubChannelConfig{AppSlug: "demo-reviewbot"},
				Slack:  &spiceboxv1alpha1.SlackChannelConfig{}},
			wantErr: "must not set spec.slack"},
		{name: "well-formed accepted",
			spec: spiceboxv1alpha1.ChannelSpec{Kind: "github", Role: spiceboxv1alpha1.ChannelRoleInput,
				GitHub: &spiceboxv1alpha1.GitHubChannelConfig{AppSlug: "demo-reviewbot"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Kind{}.ValidateSpec(&spiceboxv1alpha1.Channel{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-reviewbot-gh"}, Spec: tc.spec})
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestValidateSpecEnforcesSupportedRoles is the anti-drift assertion for the
// declaration/enforcement pair: SupportedRoles is what `oap agent lint` reads
// off a bundle's requires.channels, ValidateSpec is what reconcile enforces,
// and the Kind interface requires the second to derive from the first. A
// ValidateSpec that stopped consulting the list — or a list narrowed without
// it — would let the lint report a role the cluster then accepts, or the
// reverse.
//
// It lives here rather than in a registry-wide sweep because only this package
// can build a WELL-FORMED github Channel. A sweep can supply a kind and a role
// and nothing else, and this kind rejects such a Channel for the missing
// spec.github regardless of role, so a sweep's assert.Error would hold even
// with the role gate deleted.
//
// Every role outside the declared set is covered, so the gate cannot be
// half-removed unnoticed — the role=output case above would not have caught a
// gate that let "both" and "monitoring" through.
//
// It does NOT replace that case, and the two are not redundant: this one is
// DERIVED from SupportedRoles, so it guards the derivation (a ValidateSpec
// that stopped reading the list) while following the list wherever it goes. A
// list wrongly WIDENED to all four takes this test with it — every role
// becomes an "accepted" case and it stays green. What catches that is a case
// naming the expected outcome independently, which "role=output rejected"
// above does. Keep both.
func TestValidateSpecEnforcesSupportedRoles(t *testing.T) {
	served := Kind{}.SupportedRoles()
	require.NotEmpty(t, served)

	wellFormed := func(role string) *spiceboxv1alpha1.Channel {
		return &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-reviewbot-gh"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "github", Role: role,
				GitHub: &spiceboxv1alpha1.GitHubChannelConfig{AppSlug: "demo-reviewbot"},
			},
		}
	}

	for _, role := range spiceboxv1alpha1.AllChannelRoles() {
		if slices.Contains(served, role) {
			t.Run(role+": declared served, so a well-formed Channel is accepted", func(t *testing.T) {
				assert.NoError(t, Kind{}.ValidateSpec(wellFormed(role)))
			})
			continue
		}
		t.Run(role+": not declared served, so rejected on an otherwise well-formed Channel", func(t *testing.T) {
			err := Kind{}.ValidateSpec(wellFormed(role))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must declare role=input",
				"and the message names what this kind does serve, read from SupportedRoles")
		})
	}
}

func TestRequiredSecretKeys(t *testing.T) {
	assert.ElementsMatch(t,
		[]string{"app-id", "private-key", "webhook-secret", "installation-id"},
		Kind{}.RequiredSecretKeys(&spiceboxv1alpha1.Channel{}))
}
