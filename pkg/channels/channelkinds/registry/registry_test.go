package registry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

type stubKind struct{ name string }

func (s stubKind) Name() string                                                       { return s.name }
func (s stubKind) DefaultSessionScope() string                                        { return "auto" }
func (s stubKind) Capabilities() []string                                             { return []string{"text"} }
func (s stubKind) NewListener(_ channelkinds.Deps) channelkinds.Listener              { return nil }
func (s stubKind) NewSender(_ channelkinds.Deps) channelkinds.Sender                  { return nil }
func (s stubKind) SubChannelSender(_ string, _ channelkinds.Deps) channelkinds.Sender { return nil }
func (s stubKind) NewStreamDeltaSink(_ channelkinds.Deps) channelkinds.StreamDeltaSink {
	return nil
}
func (s stubKind) SupportsMonitoring() bool                                              { return false }
func (s stubKind) SupportsLiveViewOffer() bool                                           { return false }
func (s stubKind) NewMonitoringSender(_ channelkinds.Deps) channelkinds.MonitoringSender { return nil }
func (s stubKind) SupportedRoles() []string                                              { return spiceboxv1alpha1.AllChannelRoles() }
func (s stubKind) ValidateSpec(_ *spiceboxv1alpha1.Channel) error                        { return nil }
func (s stubKind) PublicSecretKeys(_ *spiceboxv1alpha1.Channel) []string                 { return nil }
func (s stubKind) RequiredSecretKeys(_ *spiceboxv1alpha1.Channel) []string               { return nil }
func (s stubKind) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}
func (s stubKind) RenderMention(externalID string) string                    { return externalID }
func (s stubKind) SupportedMentionLookups() []channelkinds.MentionLookupKind { return nil }
func (s stubKind) LookupUser(
	_ context.Context, _ channelkinds.LookupDeps,
	_ channelkinds.MentionLookupKind, _ string,
) (string, string, error) {
	return "", "", channelkinds.ErrMentionUnsupported
}
func (s stubKind) MentionToolDescription() string { return "" }
func (s stubKind) UserAttributable() bool         { return true }
func (s stubKind) DeliversToHuman() bool          { return true }
func (s stubKind) AllowsSyntheticIdentity() bool  { return false }
func (s stubKind) RelayedByChannelsd() bool       { return true }
func (s stubKind) SpawnsSessionOnInbound() bool   { return true }
func (s stubKind) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator {
	return nil
}
func (s stubKind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }
func (s stubKind) Wizard() channelkinds.Wizard                                    { return nil }

// restoreRegistryOnCleanup snapshots the registry's current contents and
// restores them via t.Cleanup. Tests in this file call the destructive
// Reset() to get a clean slate; without a restore, they'd permanently wipe
// out the real kinds other tests in this test binary register via blank
// import at init() (init runs exactly once, so a lost registration never
// comes back). Go links this file's internal "package registry" tests into
// the same test binary as external "package registry_test" files (like
// featuresupport_test.go) and runs the internal package's tests first, so
// without this, TestRegisterAndGet/TestRegisterDuplicatePanics would empty
// the registry before those tests ever see it.
//
// Callers of this helper MUST stay serial — no t.Parallel(). Reset() mutates
// process-wide registry state; running the snapshot/restore concurrently with
// another such test would resurrect exactly the flake this helper exists to
// fix.
func restoreRegistryOnCleanup(t *testing.T) {
	t.Helper()
	prior := All()
	t.Cleanup(func() {
		Reset()
		for _, k := range prior {
			Register(k)
		}
	})
}

func TestRegisterAndGet(t *testing.T) {
	restoreRegistryOnCleanup(t)
	Reset()
	Register(stubKind{name: "fake"})
	k, ok := Get("fake")
	require.True(t, ok, "Get(fake) should hit")
	require.NotNil(t, k)
	assert.Equal(t, "fake", k.Name())
	assert.Len(t, All(), 1)
}

func TestRegisterDuplicatePanics(t *testing.T) {
	restoreRegistryOnCleanup(t)
	Reset()
	Register(stubKind{name: "x"})
	require.Panics(t, func() {
		Register(stubKind{name: "x"})
	}, "expected panic on duplicate registration")
}

// userlessStubKind is stubKind with the one answer this table turns on
// flipped: its inbound carries no human (github and bento in production).
type userlessStubKind struct{ stubKind }

func (userlessStubKind) UserAttributable() bool { return false }

// TestIsUserlessInput pins the shared predicate several rules key off:
// "this Channel's inbound carries no human". Both halves matter — the kind's
// attribution AND the role, since the same kind in an outbound-only role
// delivers no inbound to attribute.
func TestIsUserlessInput(t *testing.T) {
	restoreRegistryOnCleanup(t)
	Reset()
	Register(userlessStubKind{stubKind{name: "userless"}})
	Register(stubKind{name: "attributable"})

	cases := []struct {
		name string
		ch   *spiceboxv1alpha1.Channel
		want bool
	}{
		{
			name: "userless kind, role=input: true",
			ch:   chOfKindRole("userless", spiceboxv1alpha1.ChannelRoleInput),
			want: true,
		},
		{
			name: "userless kind, role=both: true (it still receives)",
			ch:   chOfKindRole("userless", spiceboxv1alpha1.ChannelRoleBoth),
			want: true,
		},
		{
			name: "userless kind, role unset: true (the CRD default is both)",
			ch:   chOfKindRole("userless", ""),
			want: true,
		},
		{
			name: "userless kind, role=output: false (produces no inbound)",
			ch:   chOfKindRole("userless", spiceboxv1alpha1.ChannelRoleOutput),
			want: false,
		},
		{
			name: "userless kind, role=monitoring: false (a sink bound to no agent)",
			ch:   chOfKindRole("userless", spiceboxv1alpha1.ChannelRoleMonitoring),
			want: false,
		},
		{
			name: "attributable kind, role=input: false",
			ch:   chOfKindRole("attributable", spiceboxv1alpha1.ChannelRoleInput),
			want: false,
		},
		{
			name: "unregistered kind: false (the caller's own path refuses it by name)",
			ch:   chOfKindRole("nosuch", spiceboxv1alpha1.ChannelRoleInput),
			want: false,
		},
		{
			name: "nil Channel: false",
			ch:   nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsUserlessInput(tc.ch))
		})
	}
}

func chOfKindRole(kind, role string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		Spec: spiceboxv1alpha1.ChannelSpec{Kind: kind, Role: role},
	}
}

// TestTriggerDescriberFor_GithubYesFakeNo pins the shared lookup the
// opening_summary capability gates on: github posts opening lines for a
// triggered (userless) input and so implements channelkinds.TriggerDescriber;
// the conversational fake kind does not, and neither does an unregistered
// name. github and fake are registered by their own blank imports elsewhere
// in this package's test binary (e.g. deliverstohuman_test.go,
// browsersurface_test.go) — every init() in the binary runs before any test
// does, regardless of which file did the importing.
func TestTriggerDescriberFor_GithubYesFakeNo(t *testing.T) {
	_, ok := TriggerDescriberFor("github")
	assert.True(t, ok, "github posts opening lines, so it is a trigger describer")
	_, ok = TriggerDescriberFor("fake")
	assert.False(t, ok, "the conversational fake kind is not a trigger describer")
	_, ok = TriggerDescriberFor("does-not-exist")
	assert.False(t, ok)
}
