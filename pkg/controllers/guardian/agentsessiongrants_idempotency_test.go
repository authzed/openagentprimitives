//go:build integration

package guardian_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/guardian"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv/idempotency"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// TestReconciler_ASGReconcileConvergesViaIdempotencyHarness is the shared
// idempotency-harness counterpart of
// TestReconciler_SteadyStateReconcilePreservesObservedSchemaWrittenAt: both
// pin the SAME regression — the guardian AgentSessionGrants reconciler must
// NOT restamp status.ObservedSchemaWrittenAt on every included pass, or the
// status Patch is never a no-op and the watch re-triggers a ~5s-forever
// reconcile storm.
//
// Reuses that test's fixture shape (an MCPServer contributing the
// github_repo fragment, an AgentSessionGrants CR referencing it, debounce
// disabled so both passes reach the write+patch path) but drives
// convergence through the shared idempotency.RequireReconcileConverges
// harness instead of a hand-timed two-reconcile resourceVersion comparison
// — proving the general-purpose CI-gate harness covers this specific,
// already-fixed bug.
func TestReconciler_ASGReconcileConvergesViaIdempotencyHarness(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	mcps := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "github", Version: "1.0.0",
			Server:        spiceboxv1alpha1.MCPServerServer{URL: "https://example/mcp"},
			Tools:         []spiceboxv1alpha1.MCPServerTool{{Name: "noop"}},
			SpiceDBSchema: githubRepoFragment(),
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcps), "create MCPServer")

	io := &fakeSchemaIO{cur: ""}
	r := guardian.NewReconciler(env.Client, io, nil)
	r.BuiltinToolkits = func() []toolkit.Toolkit { return nil } // this test composes only what it declares
	// Both passes must reach the write+patch path; the 5s production
	// debounce would otherwise short-circuit the second reconcile before it
	// ever gets to patchSchemaIncluded, trivially "converging" without
	// exercising the steady-state behavior this test exists to pin.
	guardian.SetDebounceForTest(r, 0)

	asg := &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-idempotency-anchor", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionGrantsSpec{
			Pairs: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, asg), "create AgentSessionGrants")

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "cls-idempotency-anchor", Namespace: "default"}}
	idempotency.RequireReconcileConverges(t, ctx, env.Client, r, req, &spiceboxv1alpha1.AgentSessionGrants{}, 5)
}
