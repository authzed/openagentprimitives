//go:build integration

// pkg/controllers/agentsession/sidecar_pin_reconcile_test.go
//
// Reconcile-level tests for the AgentSession sidecar image-pin block:
// by-digest launch rewrite, block-mode gate, and observedPins recording.
// Pure-function coverage (UpsertObservedPin) lives in pkg/apis/v1alpha1.
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// sidecarToolboxWithImage returns a SidecarToolbox with a custom source image.
func sidecarToolboxWithImage(t *testing.T, name, image string) *spiceboxv1alpha1.SidecarToolbox {
	t.Helper()
	tb := sidecarToolbox(t, name, "", "")
	tb.Spec.Source.Image = image
	return tb
}

// stampSidecarToolboxPin writes status.pin (and optionally a PinDrift
// condition) onto an already-created SidecarToolbox.
func stampSidecarToolboxPin(t *testing.T, ctx context.Context, env *testenv.Env, tb *spiceboxv1alpha1.SidecarToolbox, pinDigest, driftCondStatus, driftReason string) {
	t.Helper()
	var live spiceboxv1alpha1.SidecarToolbox
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: tb.Namespace, Name: tb.Name}, &live))
	if pinDigest != "" {
		now := metav1.Now()
		live.Status.Pin = &spiceboxv1alpha1.PinRecord{
			Kind:       "image",
			Strength:   "named",
			Digest:     pinDigest,
			Version:    "v1",
			ObservedAt: &now,
		}
	}
	if driftCondStatus != "" {
		cond := metav1.Condition{
			Type:               spiceboxv1alpha1.PinDriftCondition,
			Status:             metav1.ConditionStatus(driftCondStatus),
			Reason:             driftReason,
			Message:            "image digest drifted",
			LastTransitionTime: metav1.Now(),
		}
		live.Status.Conditions = append(live.Status.Conditions, cond)
	}
	require.NoError(t, env.Client.Status().Update(ctx, &live))
}

// clusterImageBlockSettings creates a ClusterAgentSettings with an image-kind
// block pinning rule. The caller must delete it (or let envtest clean up).
func clusterImageBlockSettings(t *testing.T, ctx context.Context, env *testenv.Env) {
	t.Helper()
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{
				Pinning: &spiceboxv1alpha1.PinningPolicy{
					Rules: []spiceboxv1alpha1.PinningRule{
						{Kind: "image", Mode: spiceboxv1alpha1.PinModeBlock},
					},
				},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas), "create ClusterAgentSettings with image block rule")
}

// clusterImageWarnSettings creates a ClusterAgentSettings with an image-kind
// warn pinning rule.
func clusterImageWarnSettings(t *testing.T, ctx context.Context, env *testenv.Env) {
	t.Helper()
	cas := &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{
				Pinning: &spiceboxv1alpha1.PinningPolicy{
					Rules: []spiceboxv1alpha1.PinningRule{
						{Kind: "image", Mode: spiceboxv1alpha1.PinModeWarn},
					},
				},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cas), "create ClusterAgentSettings with image warn rule")
}

// TestReconcileSidecar_ByDigestLaunch_RewritesImage asserts that when a
// SidecarToolbox has a baseline digest in status.pin and the declared ref is a
// tag (not already digest-pinned), the resolved sidecar snapshot carries the
// by-digest form "repo@sha256:…".
func TestReconcileSidecar_ByDigestLaunch_RewritesImage(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	tb := sidecarToolboxWithImage(t, "pin-box", "ghcr.io/x/pin-box:v1")
	require.NoError(t, env.Client.Create(ctx, tb))
	// Stamp a baseline digest on the toolbox.
	stampSidecarToolboxPin(t, ctx, env, tb, "sha256:deadbeef", "", "")

	ac := classWithSidecars("ac-pin",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "pin-box", Ref: "pin-box"},
	)
	require.NoError(t, env.Client.Create(ctx, ac))
	markValid(t, env, ac)

	sess := validSession("s-pin", "ac-pin")
	require.NoError(t, env.Client.Create(ctx, sess))
	reconcileToWork(t, ctx, r, "s-pin")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-pin"}, &got))
	require.Len(t, got.Status.ResolvedSidecarToolboxes, 1)
	rt := got.Status.ResolvedSidecarToolboxes[0]
	// Image must be rewritten to repo@sha256:…, NOT the original :v1 tag.
	assert.Equal(t, "ghcr.io/x/pin-box@sha256:deadbeef", rt.Spec.Source.Image,
		"by-digest launch: image must be rewritten to repo@digest form")
	// Session must not fail.
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
}

// TestReconcileSidecar_ByDigestLaunch_AlreadyDigestPinnedUntouched asserts
// that a declared ref already in "@sha256:…" form is not rewritten a second
// time even when a baseline digest is recorded.
func TestReconcileSidecar_ByDigestLaunch_AlreadyDigestPinnedUntouched(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	frozenImage := "ghcr.io/x/frozen-box@sha256:aaaa1111"
	tb := sidecarToolboxWithImage(t, "frozen-box", frozenImage)
	require.NoError(t, env.Client.Create(ctx, tb))
	// Baseline digest may differ (e.g. re-verified), but the declared ref is
	// already frozen — it must not be rewritten.
	stampSidecarToolboxPin(t, ctx, env, tb, "sha256:bbbb2222", "", "")

	ac := classWithSidecars("ac-frozen",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "frozen-box", Ref: "frozen-box"},
	)
	require.NoError(t, env.Client.Create(ctx, ac))
	markValid(t, env, ac)

	sess := validSession("s-frozen", "ac-frozen")
	require.NoError(t, env.Client.Create(ctx, sess))
	reconcileToWork(t, ctx, r, "s-frozen")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-frozen"}, &got))
	require.Len(t, got.Status.ResolvedSidecarToolboxes, 1)
	rt := got.Status.ResolvedSidecarToolboxes[0]
	// Already digest-pinned — must remain unchanged.
	assert.Equal(t, frozenImage, rt.Spec.Source.Image,
		"already-digest-pinned ref must not be rewritten")
}

// TestReconcileSidecar_ByDigestLaunch_NoBaselineUntouched asserts that when
// no baseline digest is recorded (status.pin is nil), the tag ref is not
// touched.
func TestReconcileSidecar_ByDigestLaunch_NoBaselineUntouched(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	tagImage := "ghcr.io/x/tag-box:v2"
	tb := sidecarToolboxWithImage(t, "tag-box", tagImage)
	require.NoError(t, env.Client.Create(ctx, tb))
	// No baseline: status.pin stays nil.

	ac := classWithSidecars("ac-tag",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "tag-box", Ref: "tag-box"},
	)
	require.NoError(t, env.Client.Create(ctx, ac))
	markValid(t, env, ac)

	sess := validSession("s-tag", "ac-tag")
	require.NoError(t, env.Client.Create(ctx, sess))
	reconcileToWork(t, ctx, r, "s-tag")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-tag"}, &got))
	require.Len(t, got.Status.ResolvedSidecarToolboxes, 1)
	rt := got.Status.ResolvedSidecarToolboxes[0]
	// No baseline — tag must be preserved as-is.
	assert.Equal(t, tagImage, rt.Spec.Source.Image,
		"no-baseline tag ref must not be touched")
}

// TestReconcileSidecar_BlockGate_DriftedAndBlockMode asserts that when a
// SidecarToolbox has PinDrift=False/PinDrifted AND the cluster pinning policy
// mode is "block", the session is held Pending with SettingsAccepted=False
// and no runner pod is created.
func TestReconcileSidecar_BlockGate_DriftedAndBlockMode(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	// Create a cluster-level block rule for image kind.
	clusterImageBlockSettings(t, ctx, env)

	tb := sidecarToolboxWithImage(t, "drift-box", "ghcr.io/x/drift-box:v1")
	require.NoError(t, env.Client.Create(ctx, tb))
	// Stamp a baseline AND PinDrift=False/PinDrifted to simulate a drifted image.
	stampSidecarToolboxPin(t, ctx, env, tb, "sha256:original",
		string(metav1.ConditionFalse), spiceboxv1alpha1.ReasonPinDrifted)

	ac := classWithSidecars("ac-drift",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "drift-box", Ref: "drift-box"},
	)
	require.NoError(t, env.Client.Create(ctx, ac))
	markValid(t, env, ac)

	sess := validSession("s-drift", "ac-drift")
	require.NoError(t, env.Client.Create(ctx, sess))
	reconcileToWork(t, ctx, r, "s-drift")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-drift"}, &got))

	// Session must NOT be Failed (it is held Pending).
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"block gate: session must NOT be terminal-failed (it is held pending)")

	// SettingsAccepted must be False with ImagePinDrifted reason and the
	// toolbox name in the message.
	sa := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSettingsAccepted)
	require.NotNil(t, sa, "SettingsAccepted condition must be set")
	assert.Equal(t, metav1.ConditionFalse, sa.Status, "SettingsAccepted=False for block+drifted")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionImagePinDrifted, sa.Reason,
		"SettingsAccepted reason must be ImagePinDrifted")
	assert.Contains(t, sa.Message, "drift-box",
		"block gate condition message must name the toolbox")

	// No runner pod must have been created.
	var pod corev1.Pod
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-drift-runner"}, &pod)
	assert.True(t, err != nil,
		"no runner pod must exist when the session is blocked by image pin drift")
}

// TestReconcileSidecar_BlockGate_DriftedWarnMode asserts that warn mode does
// not gate the session — it proceeds normally even with PinDrift=False.
func TestReconcileSidecar_BlockGate_DriftedWarnMode(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	// Create a cluster-level warn rule for image kind (does not block).
	clusterImageWarnSettings(t, ctx, env)

	tb := sidecarToolboxWithImage(t, "warn-box", "ghcr.io/x/warn-box:v1")
	require.NoError(t, env.Client.Create(ctx, tb))
	// Stamp PinDrift=False/PinDrifted and a baseline digest.
	stampSidecarToolboxPin(t, ctx, env, tb, "sha256:original",
		string(metav1.ConditionFalse), spiceboxv1alpha1.ReasonPinDrifted)

	ac := classWithSidecars("ac-warn",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "warn-box", Ref: "warn-box"},
	)
	require.NoError(t, env.Client.Create(ctx, ac))
	markValid(t, env, ac)

	sess := validSession("s-warn", "ac-warn")
	require.NoError(t, env.Client.Create(ctx, sess))
	reconcileToWork(t, ctx, r, "s-warn")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-warn"}, &got))

	// Warn mode: session must NOT be blocked or failed.
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"warn mode: session must not boot-fail")
	sa := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSettingsAccepted)
	if sa != nil {
		assert.NotEqual(t, spiceboxv1alpha1.ReasonAgentSessionImagePinDrifted, sa.Reason,
			"warn mode: SettingsAccepted must not be set to ImagePinDrifted")
	}
	// Warn mode proceeds: the sidecar resolve loop still runs, so an
	// ObservedPin entry must have been recorded for the warn-box toolbox.
	assert.Len(t, got.Status.ObservedPins, 1,
		"warn mode: ObservedPin must be recorded even when drift is only warned (session proceeds)")
	if len(got.Status.ObservedPins) == 1 {
		assert.Equal(t, "warn-box", got.Status.ObservedPins[0].Name,
			"warn mode: ObservedPin must name the drifted toolbox")
	}
}

// TestReconcileSidecar_ObservedPins_RecordedAndUpserted asserts that:
//   - one ObservedPin entry is recorded per resolved sidecar after reconcile.
//   - a second reconcile upserts (not appends) the entry when identity is unchanged.
//   - the observation includes the correct Kind, Strength, Digest, and Version.
func TestReconcileSidecar_ObservedPins_RecordedAndUpserted(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	tb := sidecarToolboxWithImage(t, "obs-box", "ghcr.io/x/obs-box:v1")
	require.NoError(t, env.Client.Create(ctx, tb))
	// Stamp a baseline digest.
	stampSidecarToolboxPin(t, ctx, env, tb, "sha256:abc123", "", "")

	ac := classWithSidecars("ac-obs",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "obs-box", Ref: "obs-box"},
	)
	require.NoError(t, env.Client.Create(ctx, ac))
	markValid(t, env, ac)

	sess := validSession("s-obs", "ac-obs")
	require.NoError(t, env.Client.Create(ctx, sess))
	reconcileToWork(t, ctx, r, "s-obs")

	sessKey := types.NamespacedName{Namespace: "default", Name: "s-obs"}
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &got))

	// One ObservedPin for "obs-box".
	require.Len(t, got.Status.ObservedPins, 1, "one ObservedPin after first reconcile")
	op := got.Status.ObservedPins[0]
	assert.Equal(t, "obs-box", op.Name, "ObservedPin.Name must be the toolbox ref")
	assert.Equal(t, "image", op.Pin.Kind, "ObservedPin.Pin.Kind must be 'image'")
	assert.Equal(t, "named", op.Pin.Strength, "ObservedPin.Pin.Strength for :v1 tag")
	// Digest reflects the by-digest-rewritten image (the baseline digest).
	assert.Equal(t, "sha256:abc123", op.Pin.Digest,
		"ObservedPin.Pin.Digest must be the baseline digest after rewrite")
	assert.Equal(t, "v1", op.Pin.Version, "ObservedPin.Pin.Version from declared tag")
	require.NotNil(t, op.Pin.ObservedAt, "ObservedAt must be set")
	firstObservedAt := op.Pin.ObservedAt

	// Reconcile again: the same identity should be preserved (ObservedAt unchanged).
	reconcileToWork(t, ctx, r, "s-obs")

	var got2 spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, sessKey, &got2))
	require.Len(t, got2.Status.ObservedPins, 1, "upsert must not duplicate: still one entry")
	op2 := got2.Status.ObservedPins[0]
	assert.Equal(t, "obs-box", op2.Name, "Name unchanged on upsert")
	assert.Equal(t, "sha256:abc123", op2.Pin.Digest, "Digest unchanged on upsert")
	// ObservedAt preserved when identity is unchanged.
	require.NotNil(t, op2.Pin.ObservedAt)
	assert.Equal(t, firstObservedAt.Time, op2.Pin.ObservedAt.Time,
		"ObservedAt must be preserved when identity is unchanged across reconciles")
}
