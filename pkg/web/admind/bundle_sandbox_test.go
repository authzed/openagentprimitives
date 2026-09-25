package admind_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
)

// sessionDetailBody is the subset of handleSessionDetail's JSON shape this
// file exercises.
type sessionDetailBody struct {
	Bundles []admind.BundleSandbox `json:"bundles"`
}

// spiceboxSessionCR builds a SpiceboxSession fixture with the given sandbox
// handle and Ready condition, mirroring what the spiceboxsession controller
// (pkg/controllers/spiceboxsession) stamps onto status.
func spiceboxSessionCR(ns, name string, sandbox *spiceboxv1alpha1.SandboxHandle, ready bool, reason string) *spiceboxv1alpha1.SpiceboxSession {
	s := &spiceboxv1alpha1.SpiceboxSession{}
	s.Namespace, s.Name = ns, name
	s.Status.Sandbox = sandbox
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	s.Status.Conditions = []metav1.Condition{
		{Type: spiceboxv1alpha1.SpiceboxSessionConditionReady, Status: status, Reason: reason,
			LastTransitionTime: metav1.Now()},
	}
	return s
}

// withWorkspaceMode sets spec.workspace.mode on a SpiceboxSession fixture —
// spec (not status), like the AgentSession controller's BuildBundleSession
// stamps it (pkg/controllers/agentsession/bundles.go).
func withWorkspaceMode(s *spiceboxv1alpha1.SpiceboxSession, mode spiceboxv1alpha1.WorkspaceMode) *spiceboxv1alpha1.SpiceboxSession {
	s.Spec.Workspace.Mode = mode
	return s
}

func TestAdmindSessionDetail_BundlesSurfacePerBundleSandbox(t *testing.T) {
	sess := sessionCR("default", "multi-bundle", "support-bot", "Running")
	sess.Status.BundleSessions = []spiceboxv1alpha1.ResolvedBundle{
		{Name: "primary", SpiceboxSessionName: "multi-bundle-primary", AgentIdentity: "identity-a"},
		{Name: "secondary", SpiceboxSessionName: "multi-bundle-secondary", AgentIdentity: "identity-b"},
		// This bundle's SpiceboxSession does not exist yet (not created, or
		// already GC'd) — must degrade to blank sandbox fields, not an error.
		{Name: "pending", SpiceboxSessionName: "multi-bundle-pending"},
	}

	// Deliberately DIFFERENT kinds AND workspace modes across bundles of the SAME
	// session — the scenario the per-bundle (not collapsed) surfacing exists for.
	primary := withWorkspaceMode(spiceboxSessionCR("default", "multi-bundle-primary",
		&spiceboxv1alpha1.SandboxHandle{Kind: "pod", Ref: "default/pod-abc"}, true, "PodReady"),
		spiceboxv1alpha1.WorkspaceShared)
	secondary := withWorkspaceMode(spiceboxSessionCR("default", "multi-bundle-secondary",
		&spiceboxv1alpha1.SandboxHandle{Kind: "agent-sandbox", Ref: "default/claim-xyz", Prewarmed: true}, true, "Adopted"),
		spiceboxv1alpha1.WorkspaceIsolated)

	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(sess, primary, secondary).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(sess)

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/sessions/default/multi-bundle", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var body sessionDetailBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Bundles, 3, "one entry per bundle, not collapsed to a single value")

	byName := map[string]admind.BundleSandbox{}
	for _, b := range body.Bundles {
		byName[b.Name] = b
	}

	p := byName["primary"]
	assert.Equal(t, "multi-bundle-primary", p.SpiceboxSessionName)
	assert.Equal(t, "identity-a", p.AgentIdentity)
	assert.Equal(t, "pod", p.SandboxKind)
	assert.Equal(t, "default/pod-abc", p.SandboxRef)
	assert.False(t, p.Prewarmed)
	assert.Equal(t, "Ready", p.Phase)
	assert.Equal(t, "PodReady", p.Reason)
	assert.Equal(t, "shared", p.WorkspaceMode)

	s := byName["secondary"]
	assert.Equal(t, "agent-sandbox", s.SandboxKind, "a different bundle of the SAME session may resolve a different kind")
	assert.Equal(t, "default/claim-xyz", s.SandboxRef)
	assert.True(t, s.Prewarmed)
	assert.Equal(t, "Ready", s.Phase)
	assert.Equal(t, "isolated", s.WorkspaceMode, "a different bundle of the SAME session may resolve a different workspace mode")

	pending := byName["pending"]
	assert.Empty(t, pending.SandboxKind, "bundle's SpiceboxSession not found -> blank sandbox, not an error")
	assert.Empty(t, pending.SandboxRef)
	assert.Empty(t, pending.WorkspaceMode)
	assert.Equal(t, "Gone", pending.Phase,
		"an absent SpiceboxSession must say so: a blank phase renders through "+
			"sandboxSessionPhase's vocabulary as \"Unknown\" — \"not yet reconciled\" — "+
			"the opposite of what happened")
}

// TestAdmindSessionDetail_FailedBundleSandboxReportsFailedPhase covers the
// branch a plain Ready-condition read would miss: applySandboxStatus
// (pkg/controllers/spiceboxsession/conditions.go) REMOVES the Ready condition
// entirely on a terminal failure, so a naive "read the Ready condition" phase
// derivation would report "Unknown" for a failed sandbox instead of "Failed".
func TestAdmindSessionDetail_FailedBundleSandboxReportsFailedPhase(t *testing.T) {
	sess := sessionCR("default", "failed-bundle", "support-bot", "Running")
	sess.Status.BundleSessions = []spiceboxv1alpha1.ResolvedBundle{
		{Name: "primary", SpiceboxSessionName: "failed-bundle-primary"},
	}
	failed := &spiceboxv1alpha1.SpiceboxSession{}
	failed.Namespace, failed.Name = "default", "failed-bundle-primary"
	failed.Status.Sandbox = &spiceboxv1alpha1.SandboxHandle{Kind: "pod", Ref: "default/pod-dead"}
	// Mirrors applySandboxStatus's PhaseFailed branch: Failed=True, no Ready
	// condition at all (removed, not set False).
	failed.Status.Conditions = []metav1.Condition{
		{Type: spiceboxv1alpha1.SpiceboxSessionConditionFailed, Status: metav1.ConditionTrue,
			Reason: "PodCrashed", LastTransitionTime: metav1.Now()},
	}

	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess, failed).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(sess)

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/sessions/default/failed-bundle", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var body sessionDetailBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Bundles, 1)
	assert.Equal(t, "Failed", body.Bundles[0].Phase, "Failed must win even though Ready is absent, not False")
	assert.Equal(t, "PodCrashed", body.Bundles[0].Reason)
}

func TestAdmindSessionDetail_NoBundlesOmitsBundlesField(t *testing.T) {
	sess := sessionCR("default", "no-bundles", "support-bot", "Running")
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(sess)

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/sessions/default/no-bundles", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), `"bundles"`, "a session with no resolved bundles omits the field entirely")
}
