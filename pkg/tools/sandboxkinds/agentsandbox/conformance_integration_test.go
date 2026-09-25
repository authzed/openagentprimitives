//go:build integration

package agentsandbox_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	execfake "github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/agentsandbox"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/conformance"
)

// The second real backend runs the same contract as the first, against a real
// API server.
//
// LIMIT, stated so nobody reads more into a green run than it earns: envtest
// runs no agent-sandbox CONTROLLER. A Sandbox created here never becomes
// Ready, never gets its pod-name annotation, and never produces a pod. This
// covers Ensure, idempotency, Teardown, the Watches shape, the
// not-found/terminating arms of Status, and Executor's fail-closed refusal
// when no pod is bound yet. It cannot cover PhaseReady or a successful
// Executor bind; those need a live cluster with the controller installed.
//
// The same LIMIT applies to the Prewarmer rows added below: with no
// controller running, no SandboxClaim is ever bound and no pre-warmed
// sandbox is ever ADOPTED by a session. Handle.Prewarmed never turns true
// here, and that is NOT exercised by this test.
//
// What IS asserted, against a real API server: ReconcilePool and
// SweepOrphanedPools are idempotent and accept the shapes their own doc
// comments promise (Replicas: 0, an empty desiredNamespaces) without
// erroring; and ReconcilePool REFUSES an empty PoolRequest.ClassUID rather
// than creating a pool nothing owns. Beyond that, CountPrewarmPoolObjects below counts
// SandboxWarmPool/SandboxTemplate objects owned by the class (by
// ownerReference, the same field ReconcilePool itself stamps and checks —
// see hasOwnerRef in prewarm.go) rather than conformance.go importing
// agent-sandbox's own types, and the conformance suite uses it to prove
// SweepOrphanedPools OBSERVABLY reclaims a namespace dropped from the
// desired set (object count goes from positive to zero via a genuine
// apiserver Delete) while leaving a namespace still desired untouched
// (count unchanged). That is a real create-then-delete round trip, not an
// inference from a nil error.
//
// What is NOT asserted here: whether a converged ReconcilePool call
// (identical request, already applied) skips its Update write entirely.
// That needs a ResourceVersion-equality check, which only
// prewarm_test.go's fake-client suite makes
// (TestReconcilePool_IsIdempotent) — this file's idempotency rows prove
// "no error and no broken state on repeat," not "no write occurred."
func TestConformance_AgentSandbox(t *testing.T) {
	env := testenv.Start(t)

	rt, err := agentsandbox.Kind{}.NewRuntime(sandboxkinds.Deps{
		Client: env.Client, ExecFor: execfake.New().For,
	})
	require.NoError(t, err,
		"the agent-sandbox CRDs must be installed in testenv; a NoKindMatch here "+
			"means agentSandboxCRDs() failed to load them")

	var n int
	conformance.Run(t, conformance.Subject{
		Name:    "agent-sandbox",
		Runtime: rt,
		NewRequest: func() sandboxkinds.EnsureRequest {
			// A distinct session per subtest: sandboxes persist in the shared
			// API server, so reusing one name would let cases collide.
			n++
			return sandboxkinds.EnsureRequest{
				Session: &v1alpha1.SpiceboxSession{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("demo-session-%d", n),
						Namespace: "default",
						// podspec.Build stamps this into the owner reference,
						// and a real API server rejects an empty owner UID.
						UID: types.UID(fmt.Sprintf("demo-session-%d-uid", n)),
					},
					Spec: v1alpha1.SpiceboxSessionSpec{Class: "demo-class"},
				},
				Class: v1alpha1.SpiceboxClassSpec{
					Image: "demo.invalid/spicebox-sandbox:test",
					Resources: v1alpha1.SpiceboxResources{
						CPU:              resource.MustParse("1"),
						Memory:           resource.MustParse("256Mi"),
						EphemeralStorage: resource.MustParse("1Gi"),
					},
				},
			}
		},
		// No agent-sandbox controller runs here, so nothing ever goes Ready.
		ExpectReadyAfterEnsure: false,
		// The controller-stamped pod-name annotation never arrives under
		// envtest, so Executor must refuse rather than bind to nothing.
		ExecutorNeedsRunningSandbox: true,
		PrewarmPoolRequest: func() sandboxkinds.PoolRequest {
			// A distinct class name per subtest: poolNameFor is a pure function
			// of ClassName alone and stays stable across edits by design, so
			// reusing one name would collide two subtests on the same pool.
			n++
			className := fmt.Sprintf("demo-class-%d", n)
			return sandboxkinds.PoolRequest{
				ClassName: className,
				Namespace: "default",
				ClassUID:  types.UID(className + "-uid"),
				Class: v1alpha1.SpiceboxClassSpec{
					Image: "demo.invalid/spicebox-sandbox:test",
					Resources: v1alpha1.SpiceboxResources{
						CPU:              resource.MustParse("1"),
						Memory:           resource.MustParse("256Mi"),
						EphemeralStorage: resource.MustParse("1Gi"),
					},
				},
				Replicas: 2,
			}
		},
		// Counts SandboxWarmPool + SandboxTemplate objects owned by className in
		// namespace, so conformance.go can prove SweepOrphanedPools actually
		// deletes objects rather than merely returning nil. Ownership is decided
		// by ownerReference (Kind + Name), matching the SAME field
		// ReconcilePool stamps (classOwnerRef) and SweepOrphanedPools itself
		// checks (hasOwnerRef) before deleting — not classLabelKey, which is
		// unexported and unavailable to this external test package, and which
		// prewarm.go's own doc comment already treats as a fast index rather
		// than the authority.
		CountPrewarmPoolObjects: func(ctx context.Context, namespace, className string) (int, error) {
			total := 0

			var pools sandboxextv1beta1.SandboxWarmPoolList
			if err := env.Client.List(ctx, &pools, client.InNamespace(namespace)); err != nil {
				return 0, fmt.Errorf("list sandbox warm pools: %w", err)
			}
			for i := range pools.Items {
				if ownedByClass(pools.Items[i].OwnerReferences, className) {
					total++
				}
			}

			var templates sandboxextv1beta1.SandboxTemplateList
			if err := env.Client.List(ctx, &templates, client.InNamespace(namespace)); err != nil {
				return 0, fmt.Errorf("list sandbox templates: %w", err)
			}
			for i := range templates.Items {
				if ownedByClass(templates.Items[i].OwnerReferences, className) {
					total++
				}
			}

			return total, nil
		},
	})

	// Guard against the fixture drifting into a shape envtest silently accepts.
	var list sandboxv1beta1.SandboxList
	require.NoError(t, env.Client.List(t.Context(), &list))
	require.NotEmpty(t, list.Items, "conformance must have created at least one Sandbox")
}

// ownedByClass reports whether refs names className as a SpiceboxClass
// owner. Matches the shape (Kind + Name) that classOwnerRef in prewarm.go
// stamps onto every object ReconcilePool creates.
func ownedByClass(refs []metav1.OwnerReference, className string) bool {
	for _, r := range refs {
		if r.Kind == "SpiceboxClass" && r.Name == className {
			return true
		}
	}
	return false
}
