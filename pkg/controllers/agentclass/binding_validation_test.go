// Package agentclass_test exercises the InformationLeakageReady condition
// that the AgentClass controller sets based on whether each bound Channel's
// kind satisfies the capability level required by the policy.
package agentclass_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// ---------------------------------------------------------------------------
// Fake kind types for info-leakage binding tests.
//
// Three variants are registered once at package init() under stable names:
//   "il-full"       — AudienceResolver with CapabilityFull
//   "il-singleuser" — AudienceResolver with CapabilitySingleUser
//   "il-unsup"      — no AudienceResolver (CapabilityUnsupported)
//
// Tests use fake.NewClientBuilder() so no CRD enum validation fires —
// the kind names need not appear in the Channel CRD's spec.kind enum.
// ---------------------------------------------------------------------------

// ilFullKind — CapabilityFull AudienceResolver.
type ilFullKind struct{}

func (ilFullKind) Name() string                                                        { return "il-full" }
func (ilFullKind) DefaultSessionScope() string                                         { return "user" }
func (ilFullKind) Capabilities() []string                                              { return []string{"text"} }
func (ilFullKind) NewListener(channelkinds.Deps) channelkinds.Listener                 { return nil }
func (ilFullKind) NewSender(channelkinds.Deps) channelkinds.Sender                     { return nil }
func (ilFullKind) SubChannelSender(string, channelkinds.Deps) channelkinds.Sender      { return nil }
func (ilFullKind) NewStreamDeltaSink(channelkinds.Deps) channelkinds.StreamDeltaSink   { return nil }
func (ilFullKind) SupportsMonitoring() bool                                            { return false }
func (ilFullKind) SupportsLiveViewOffer() bool                                         { return false }
func (ilFullKind) NewMonitoringSender(channelkinds.Deps) channelkinds.MonitoringSender { return nil }
func (ilFullKind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator {
	return nil
}
func (ilFullKind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }
func (ilFullKind) SupportedRoles() []string                                       { return spiceboxv1alpha1.AllChannelRoles() }
func (ilFullKind) ValidateSpec(*spiceboxv1alpha1.Channel) error                   { return nil }
func (ilFullKind) PublicSecretKeys(*spiceboxv1alpha1.Channel) []string            { return nil }
func (ilFullKind) RequiredSecretKeys(*spiceboxv1alpha1.Channel) []string          { return nil }
func (ilFullKind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}
func (ilFullKind) RenderMention(externalID string) string                    { return externalID }
func (ilFullKind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return nil }
func (ilFullKind) LookupUser(context.Context, channelkinds.LookupDeps, channelkinds.MentionLookupKind, string) (string, string, error) {
	return "", "", channelkinds.ErrMentionUnsupported
}
func (ilFullKind) MentionToolDescription() string              { return "" }
func (ilFullKind) UserAttributable() bool                      { return true }
func (ilFullKind) DeliversToHuman() bool                       { return true }
func (ilFullKind) AllowsSyntheticIdentity() bool               { return false }
func (ilFullKind) RelayedByChannelsd() bool                    { return true }
func (ilFullKind) SpawnsSessionOnInbound() bool                { return true }
func (ilFullKind) Wizard() channelkinds.Wizard                 { return nil }
func (ilFullKind) AudienceCapability() channelkinds.Capability { return channelkinds.CapabilityFull }
func (ilFullKind) ResolveAudience(context.Context, channelkinds.SessionInfo) ([]string, error) {
	return nil, nil
}

// ilSingleUserKind — CapabilitySingleUser AudienceResolver.
type ilSingleUserKind struct{}

func (ilSingleUserKind) Name() string                                                   { return "il-singleuser" }
func (ilSingleUserKind) DefaultSessionScope() string                                    { return "user" }
func (ilSingleUserKind) Capabilities() []string                                         { return []string{"text"} }
func (ilSingleUserKind) NewListener(channelkinds.Deps) channelkinds.Listener            { return nil }
func (ilSingleUserKind) NewSender(channelkinds.Deps) channelkinds.Sender                { return nil }
func (ilSingleUserKind) SubChannelSender(string, channelkinds.Deps) channelkinds.Sender { return nil }
func (ilSingleUserKind) NewStreamDeltaSink(channelkinds.Deps) channelkinds.StreamDeltaSink {
	return nil
}
func (ilSingleUserKind) SupportsMonitoring() bool    { return false }
func (ilSingleUserKind) SupportsLiveViewOffer() bool { return false }

func (ilSingleUserKind) NewMonitoringSender(channelkinds.Deps) channelkinds.MonitoringSender {
	return nil
}
func (ilSingleUserKind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator {
	return nil
}
func (ilSingleUserKind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver {
	return nil
}
func (ilSingleUserKind) SupportedRoles() []string                              { return spiceboxv1alpha1.AllChannelRoles() }
func (ilSingleUserKind) ValidateSpec(*spiceboxv1alpha1.Channel) error          { return nil }
func (ilSingleUserKind) PublicSecretKeys(*spiceboxv1alpha1.Channel) []string   { return nil }
func (ilSingleUserKind) RequiredSecretKeys(*spiceboxv1alpha1.Channel) []string { return nil }
func (ilSingleUserKind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}
func (ilSingleUserKind) RenderMention(externalID string) string                    { return externalID }
func (ilSingleUserKind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return nil }
func (ilSingleUserKind) LookupUser(context.Context, channelkinds.LookupDeps, channelkinds.MentionLookupKind, string) (string, string, error) {
	return "", "", channelkinds.ErrMentionUnsupported
}
func (ilSingleUserKind) MentionToolDescription() string { return "" }
func (ilSingleUserKind) UserAttributable() bool         { return true }
func (ilSingleUserKind) DeliversToHuman() bool          { return true }
func (ilSingleUserKind) AllowsSyntheticIdentity() bool  { return false }
func (ilSingleUserKind) RelayedByChannelsd() bool       { return true }
func (ilSingleUserKind) SpawnsSessionOnInbound() bool   { return true }
func (ilSingleUserKind) Wizard() channelkinds.Wizard    { return nil }
func (ilSingleUserKind) AudienceCapability() channelkinds.Capability {
	return channelkinds.CapabilitySingleUser
}
func (ilSingleUserKind) ResolveAudience(context.Context, channelkinds.SessionInfo) ([]string, error) {
	return nil, nil
}

// ilUnsupKind — no AudienceResolver (CapabilityUnsupported).
type ilUnsupKind struct{}

func (ilUnsupKind) Name() string                                                        { return "il-unsup" }
func (ilUnsupKind) DefaultSessionScope() string                                         { return "user" }
func (ilUnsupKind) Capabilities() []string                                              { return []string{"text"} }
func (ilUnsupKind) NewListener(channelkinds.Deps) channelkinds.Listener                 { return nil }
func (ilUnsupKind) NewSender(channelkinds.Deps) channelkinds.Sender                     { return nil }
func (ilUnsupKind) SubChannelSender(string, channelkinds.Deps) channelkinds.Sender      { return nil }
func (ilUnsupKind) NewStreamDeltaSink(channelkinds.Deps) channelkinds.StreamDeltaSink   { return nil }
func (ilUnsupKind) SupportsMonitoring() bool                                            { return false }
func (ilUnsupKind) SupportsLiveViewOffer() bool                                         { return false }
func (ilUnsupKind) NewMonitoringSender(channelkinds.Deps) channelkinds.MonitoringSender { return nil }
func (ilUnsupKind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator {
	return nil
}
func (ilUnsupKind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }
func (ilUnsupKind) SupportedRoles() []string                                       { return spiceboxv1alpha1.AllChannelRoles() }
func (ilUnsupKind) ValidateSpec(*spiceboxv1alpha1.Channel) error                   { return nil }
func (ilUnsupKind) PublicSecretKeys(*spiceboxv1alpha1.Channel) []string            { return nil }
func (ilUnsupKind) RequiredSecretKeys(*spiceboxv1alpha1.Channel) []string          { return nil }
func (ilUnsupKind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}
func (ilUnsupKind) RenderMention(externalID string) string                    { return externalID }
func (ilUnsupKind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return nil }
func (ilUnsupKind) LookupUser(context.Context, channelkinds.LookupDeps, channelkinds.MentionLookupKind, string) (string, string, error) {
	return "", "", channelkinds.ErrMentionUnsupported
}
func (ilUnsupKind) MentionToolDescription() string { return "" }
func (ilUnsupKind) UserAttributable() bool         { return true }
func (ilUnsupKind) DeliversToHuman() bool          { return true }
func (ilUnsupKind) AllowsSyntheticIdentity() bool  { return false }
func (ilUnsupKind) RelayedByChannelsd() bool       { return true }
func (ilUnsupKind) SpawnsSessionOnInbound() bool   { return true }
func (ilUnsupKind) Wizard() channelkinds.Wizard    { return nil }

// init registers the three stable test kinds. Uses the idempotent guard
// so re-linking this test package alongside controller_test.go (which also
// calls init) doesn't panic on duplicate registration.
func init() {
	for _, k := range []channelkinds.Kind{ilFullKind{}, ilSingleUserKind{}, ilUnsupKind{}} {
		if _, ok := chregistry.Get(k.Name()); !ok {
			chregistry.Register(k)
		}
	}
}

// ---------------------------------------------------------------------------
// Table-driven test
// ---------------------------------------------------------------------------

// TestAgentClass_InfoLeakageBindingValidation verifies the
// InformationLeakageReady condition across all relevant policy × capability
// combinations. It uses a fake client (not envtest) so that the kind names
// used here (il-full, il-singleuser, il-unsup) don't need to appear in the
// Channel CRD's spec.kind validation enum.
func TestAgentClass_InfoLeakageBindingValidation(t *testing.T) {
	type wantCond struct {
		status metav1.ConditionStatus
		reason string // empty → don't assert reason
	}

	cases := []struct {
		name             string
		mode             string // "disabled" | "logging" | "enforcing"
		channelKind      string // "il-full", "il-singleuser", "il-unsup"
		onUnsupported    string // "" → default ("blockBinding")
		singleUserBypass *bool  // nil → default (true)
		want             wantCond
	}{
		// -------------------------------------------------------------------
		// mode=disabled: always Ready=True regardless of capability
		// -------------------------------------------------------------------
		{
			name:        "disabled + Unsupported → Ready=True",
			mode:        "disabled",
			channelKind: "il-unsup",
			want:        wantCond{status: metav1.ConditionTrue},
		},
		{
			name:             "disabled + SingleUser + bypass=false → Ready=True",
			mode:             "disabled",
			channelKind:      "il-singleuser",
			singleUserBypass: boolPtr(false),
			want:             wantCond{status: metav1.ConditionTrue},
		},
		{
			name:        "disabled + Full → Ready=True",
			mode:        "disabled",
			channelKind: "il-full",
			want:        wantCond{status: metav1.ConditionTrue},
		},

		// -------------------------------------------------------------------
		// mode=enforcing + CapabilityUnsupported
		// -------------------------------------------------------------------
		{
			name:          "enforcing + Unsupported + blockBinding → Ready=False/ChannelKindLacksAudienceResolver",
			mode:          "enforcing",
			channelKind:   "il-unsup",
			onUnsupported: "blockBinding",
			want: wantCond{
				status: metav1.ConditionFalse,
				reason: spiceboxv1alpha1.ReasonChannelKindLacksAudienceResolver,
			},
		},
		{
			name:        "enforcing + Unsupported + default onUnsupported → Ready=False (default=blockBinding)",
			mode:        "enforcing",
			channelKind: "il-unsup",
			// onUnsupported omitted → defaults to "blockBinding"
			want: wantCond{
				status: metav1.ConditionFalse,
				reason: spiceboxv1alpha1.ReasonChannelKindLacksAudienceResolver,
			},
		},
		{
			name:          "enforcing + Unsupported + logOnly → Ready=True",
			mode:          "enforcing",
			channelKind:   "il-unsup",
			onUnsupported: "logOnly",
			want:          wantCond{status: metav1.ConditionTrue},
		},
		{
			name:          "enforcing + Unsupported + bypass → Ready=True",
			mode:          "enforcing",
			channelKind:   "il-unsup",
			onUnsupported: "bypass",
			want:          wantCond{status: metav1.ConditionTrue},
		},

		// -------------------------------------------------------------------
		// mode=enforcing + CapabilitySingleUser
		// -------------------------------------------------------------------
		{
			name:             "enforcing + SingleUser + bypass=true → Ready=True",
			mode:             "enforcing",
			channelKind:      "il-singleuser",
			singleUserBypass: boolPtr(true),
			want:             wantCond{status: metav1.ConditionTrue},
		},
		{
			name:             "enforcing + SingleUser + bypass=false → Ready=False/SingleUserBypassDisabled",
			mode:             "enforcing",
			channelKind:      "il-singleuser",
			singleUserBypass: boolPtr(false),
			want: wantCond{
				status: metav1.ConditionFalse,
				reason: spiceboxv1alpha1.ReasonSingleUserBypassDisabled,
			},
		},
		{
			name:        "enforcing + SingleUser + default bypass (true) → Ready=True",
			mode:        "enforcing",
			channelKind: "il-singleuser",
			// singleUserBypass omitted → default true
			want: wantCond{status: metav1.ConditionTrue},
		},

		// -------------------------------------------------------------------
		// mode=enforcing + CapabilityFull
		// -------------------------------------------------------------------
		{
			name:        "enforcing + Full → Ready=True",
			mode:        "enforcing",
			channelKind: "il-full",
			want:        wantCond{status: metav1.ConditionTrue},
		},

		// -------------------------------------------------------------------
		// mode=logging: always Ready=True (warning is logged, not blocking)
		// -------------------------------------------------------------------
		{
			name:          "logging + Unsupported + blockBinding → Ready=True (warn only)",
			mode:          "logging",
			channelKind:   "il-unsup",
			onUnsupported: "blockBinding",
			want:          wantCond{status: metav1.ConditionTrue},
		},
		{
			name:             "logging + SingleUser + bypass=false → Ready=True (warn only)",
			mode:             "logging",
			channelKind:      "il-singleuser",
			singleUserBypass: boolPtr(false),
			want:             wantCond{status: metav1.ConditionTrue},
		},
		{
			name:        "logging + Full → Ready=True",
			mode:        "logging",
			channelKind: "il-full",
			want:        wantCond{status: metav1.ConditionTrue},
		},
	}

	scheme := testfixtures.NewScheme(t)

	for i, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			// Use a per-test fake client (no CRD enum validation).
			acName := fmt.Sprintf("ac-il-%d", i)
			chName := fmt.Sprintf("ch-il-%d", i)

			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("sk-test")},
			}

			policy := &spiceboxv1alpha1.InformationLeakagePolicy{Mode: tc.mode}
			if tc.onUnsupported != "" {
				policy.OnUnsupportedChannel = tc.onUnsupported
			}
			if tc.singleUserBypass != nil {
				policy.SingleUserBypass = tc.singleUserBypass
			}

			ac := newClass(acName)
			ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
				InformationLeakage: policy,
			}

			ch := &spiceboxv1alpha1.Channel{
				ObjectMeta: metav1.ObjectMeta{Name: chName, Namespace: "default"},
				Spec: spiceboxv1alpha1.ChannelSpec{
					Kind:           tc.channelKind,
					AgentClass:     acName,
					SessionScope:   "auto",
					CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "llm-creds"},
				},
			}

			c := fakeclient.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(sec, ac, ch).
				WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
				Build()
			r := &agentclass.Reconciler{Client: c, APIReader: c}

			// Reconcile computes BoundChannels then calls
			// reconcileInfoLeakageCondition.
			_, err := r.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: "default", Name: acName},
			})
			require.NoError(t, err, "Reconcile must not error")

			// Fetch the updated AgentClass status.
			var got spiceboxv1alpha1.AgentClass
			require.NoError(t, c.Get(ctx,
				types.NamespacedName{Namespace: "default", Name: acName}, &got),
				"Get AgentClass after reconcile")

			// Assert the InformationLeakageReady condition.
			cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionInformationLeakageReady)
			require.NotNil(t, cond,
				"InformationLeakageReady condition must be present; all conditions=%+v",
				got.Status.Conditions)
			assert.Equal(t, tc.want.status, cond.Status,
				"InformationLeakageReady.Status")
			if tc.want.reason != "" {
				assert.Equal(t, tc.want.reason, cond.Reason,
					"InformationLeakageReady.Reason")
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
