// pkg/channels/channelsd/pipeline/session_release_interaction_test.go
//
// decideSessionRelease — the bound decision handler for the session_release
// interaction category. Unlike this package's other bound-handler tests
// (which call p.decideX directly, since the generic pipe already ran the
// category's standing check before invoking it), THIS file's second test goes
// through the actual registry dispatch path
// (channelinteractions.Bind -> channelinteractions.HandlerFor -> invoke) —
// the seam this file exists to cover: the decision-application logic lives in
// pkg/controllers/sessionhold.Reconciler.Decide (already unit-tested in
// isolation there), and what was missing before this file existed was
// anything in channelsd routing a real click to it at all.
package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories/sessionrelease"
)

// registerRealSessionReleaseCategory re-registers the production
// session_release row after resetInteractions has cleared it mid-test —
// mirrors TestBindPermissionHandler_RegistersDecidePermission's identical
// need, since resetInteractions leaves the registry empty until its
// t.Cleanup runs at the end of the test, and channelinteractions.Bind panics
// on an unregistered category.
func registerRealSessionReleaseCategory(t *testing.T) {
	t.Helper()
	channelinteractions.Register(channelinteractions.Category{
		Name:      sessionrelease.CategoryName,
		Tone:      channelinteractions.ToneCritical,
		Deciders:  channelinteractions.DecideOwner,
		Resume:    channelinteractions.ResumeNone,
		Resurface: channelinteractions.ResurfaceNone,
		Surface:   channelinteractions.SurfaceDMOnly,
	})
}

// TestBindSessionReleaseHandler_RegistersDecideSessionRelease mirrors
// TestBindPermissionHandler_RegistersDecidePermission /
// TestBindQueuedInterruptHandler's pattern exactly: proves
// BindSessionReleaseHandler wires a non-nil handler for session_release,
// the "Bind at process start" deliverable internal/cmd/channelsd/main.go
// relies on.
func TestBindSessionReleaseHandler_RegistersDecideSessionRelease(t *testing.T) {
	resetInteractions(t)
	registerRealSessionReleaseCategory(t)
	p, _, _, _, _ := newPipeline(t)

	BindSessionReleaseHandler(p)

	h, ok := channelinteractions.HandlerFor(sessionrelease.CategoryName)
	require.True(t, ok, "BindSessionReleaseHandler must bind a handler for session_release")
	require.NotNil(t, h, "bound handler must not be nil")
}

// TestSessionReleaseDecision_dispatchedThroughTheBoundHandlerReachesSessionHold
// is the seam test: it does NOT call p.decideSessionRelease directly. It
// binds via BindSessionReleaseHandler (the real process-start wiring), looks
// the handler back up via channelinteractions.HandlerFor (the same lookup
// HandleInteractionDecision performs on every real click), and invokes IT —
// proving a decision routed through the registry actually reaches
// pkg/controllers/sessionhold.Reconciler.Decide and mutates the real
// SessionHold object sitting behind this Pipeline's own K8s client. A test
// that only called p.decideSessionRelease (or sessionhold.Reconciler.Decide)
// directly would pass even if BindSessionReleaseHandler were never called at
// all — exactly the defect this file exists to fix.
func TestSessionReleaseDecision_dispatchedThroughTheBoundHandlerReachesSessionHold(t *testing.T) {
	resetInteractions(t)
	registerRealSessionReleaseCategory(t)

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-hold", Namespace: "demo-ns"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo-ns", Name: "demo-session"},
			Reason:     "manual review",
			Source:     "manual",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{
			Phase:          spiceboxv1alpha1.SessionHoldPhaseActive,
			InteractionRef: "req-1",
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(hold).
		WithStatusSubresource(&spiceboxv1alpha1.SessionHold{}).
		Build()

	p, _, _, _ := newPipelineOn(t, cli)
	BindSessionReleaseHandler(p)

	h, ok := channelinteractions.HandlerFor(sessionrelease.CategoryName)
	require.True(t, ok, "session_release must be bound before a decision can be dispatched")

	out, err := h(context.Background(), channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: "demo-ns", Name: "demo-session"},
		Payload: channelevents.InteractionDecisionPayload{
			Category:   sessionrelease.CategoryName,
			RequestRef: "req-1",
			ActionID:   "refuse",
			Decider:    channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_OWNER"},
		},
	})
	require.NoError(t, err, "dispatched session_release decision")
	assert.Equal(t, channelevents.OutcomeDenied, out.Result)

	var got spiceboxv1alpha1.SessionHold
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: "demo-ns", Name: "demo-hold"}, &got))
	assert.Equal(t, spiceboxv1alpha1.SessionHoldPhaseActive, got.Status.Phase,
		"a refusal leaves the hold Active")
	assert.Contains(t, got.Status.Determination, "refused",
		"the SAME SessionHold object this Pipeline's K8s client holds was mutated by the dispatched decision — proving the bound handler reached pkg/controllers/sessionhold.Reconciler.Decide, not a disconnected copy")
}
