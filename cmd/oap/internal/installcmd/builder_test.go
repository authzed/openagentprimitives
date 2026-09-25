package installcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestApplyBuilderBundle_InstallsLockedClassIntoSystemNS proves
// applyBuilderBundle lands the embedded agent-builder bundle at the FIXED
// agentprimitives-system/agent-builder location, with the passed starters
// overlaid onto spec.authz.session.allowedStarters (via oap.Answers, not
// InstallOpts.Sets — see builder.go's comment on why a []string answer is
// required for a resourceList question) and the class's lockdown invariants
// (no platform-admin fallback, starters-only interact, no interactPermission)
// intact from the embedded manifest, plus the workshop sidecar cross-ref that
// Task 5's seeding depends on.
func TestApplyBuilderBundle_InstallsLockedClassIntoSystemNS(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build() // installcmd's harness; SSA works on the fake client
	res, err := applyBuilderBundle(ctx, c, []string{"user:demo-admin"})
	require.NoError(t, err)
	require.Equal(t, "agent-builder", res.Name)

	var ac spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "agentprimitives-system", Name: "agent-builder"}, &ac))
	assert.Equal(t, []string{"user:demo-admin"}, ac.Spec.Authz.Session.AllowedStarters)
	assert.False(t, derefBool(ac.Spec.Authz.Session.PlatformAdminsMayStart))
	assert.True(t, ac.Spec.Authz.Session.OnlyStartersInteract)
	assert.Empty(t, ac.Spec.Authz.Session.InteractPermission)
	// the load-bearing cross-ref: sidecarToolboxes[].ref == "workshop"
	require.Len(t, ac.Spec.SidecarToolboxes, 1)
	assert.Equal(t, "workshop", ac.Spec.SidecarToolboxes[0].Ref)
}

// TestApplyBuilderBundle_NoStartersIsCallersResponsibility documents that
// applyBuilderBundle itself does not special-case an empty starters slice —
// the "no silent skip" refusal lives at the install.go call site (the flag
// wiring), because THIS function's contract is "apply with the given
// starters", not "decide whether the builder should be installed at all".
// An empty slice here still writes a bundle, just with allowedStarters
// cleared out — proving the overlay path itself never silently substitutes a
// default.
func TestApplyBuilderBundle_NoStartersIsCallersResponsibility(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()
	res, err := applyBuilderBundle(ctx, c, nil)
	require.NoError(t, err)
	require.Equal(t, "agent-builder", res.Name)

	var ac spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "agentprimitives-system", Name: "agent-builder"}, &ac))
	assert.Empty(t, ac.Spec.Authz.Session.AllowedStarters)
}

// derefBool returns false for a nil *bool rather than panicking, so this test
// can assert against PlatformAdminsMayStart's dereferenced value regardless
// of whether the manifest left the pointer nil or set it explicitly false.
func derefBool(b *bool) bool {
	return b != nil && *b
}

// TestSeedBuilderClass_MergesIntoExistingSettings proves seedBuilderClass
// merges the builder's sanction entry onto a PRE-EXISTING ClusterAgentSettings
// (the common post-`oap init` case, where a pinning policy is already set)
// rather than clobbering it — and that a second seed does not duplicate the
// entry, since applyBuilderBundle + seedBuilderClass both run on every
// install, not just the first.
func TestSeedBuilderClass_MergesIntoExistingSettings(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()
	// A prior CAS with pinning already set (the common post-`oap init` case).
	prior := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{Limits: &spiceboxv1alpha1.SettingsLimits{
			Pinning: &spiceboxv1alpha1.PinningPolicy{Rules: []spiceboxv1alpha1.PinningRule{{Kind: "AgentClass", Mode: "digest"}}},
		}},
	}
	require.NoError(t, c.Create(ctx, prior))

	require.NoError(t, seedBuilderClass(ctx, c))

	var got spiceboxv1alpha1.ClusterAgentSettings
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &got))
	require.NotNil(t, got.Spec.Limits.BuilderClasses)
	assert.Equal(t, []spiceboxv1alpha1.BuilderClassRef{{
		Namespace: "agentprimitives-system", Name: "agent-builder", SidecarToolbox: "workshop",
	}}, *got.Spec.Limits.BuilderClasses)
	// the pre-existing pinning survived (proves merge, not clobber)
	require.NotNil(t, got.Spec.Limits.Pinning)
	assert.Len(t, got.Spec.Limits.Pinning.Rules, 1)

	// idempotent: a second seed does not duplicate the entry
	require.NoError(t, seedBuilderClass(ctx, c))
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &got))
	assert.Len(t, *got.Spec.Limits.BuilderClasses, 1)
}

// TestSeedBuilderClass_CreatesWhenAbsent proves seedBuilderClass creates a
// fresh ClusterAgentSettings singleton (rather than erroring) when no CAS
// exists yet — the case where oap install runs before oap init's settings
// wizard has ever created one.
func TestSeedBuilderClass_CreatesWhenAbsent(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()

	require.NoError(t, seedBuilderClass(ctx, c))

	var got spiceboxv1alpha1.ClusterAgentSettings
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &got))
	require.NotNil(t, got.Spec.Limits)
	require.NotNil(t, got.Spec.Limits.BuilderClasses)
	assert.Equal(t, []spiceboxv1alpha1.BuilderClassRef{{
		Namespace: "agentprimitives-system", Name: "agent-builder", SidecarToolbox: "workshop",
	}}, *got.Spec.Limits.BuilderClasses)
}
