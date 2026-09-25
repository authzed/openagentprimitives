// Package conformance holds the assertions every sandbox backend must satisfy.
// A new backend proves itself by passing conformance.Run, so the contract lives
// in executable form rather than in prose a backend author may not read.
package conformance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// Subject is one backend under test.
type Subject struct {
	// Name labels the subtests.
	Name string
	// Runtime is the backend's runtime, ready to use.
	Runtime sandboxkinds.Runtime
	// NewRequest returns a fresh EnsureRequest for this backend. Called per
	// subtest so cases cannot leak state into each other.
	NewRequest func() sandboxkinds.EnsureRequest
	// ExpectReadyAfterEnsure is true for a backend whose sandbox is usable the
	// moment Ensure returns. A Kubernetes backend is not: the pod still has to
	// schedule, so it reports Pending and the controller waits on a watch.
	ExpectReadyAfterEnsure bool
	// ExecutorNeedsRunningSandbox is true for a backend that cannot name its
	// exec target until the sandbox is actually running. agent-sandbox reads
	// the pod from an annotation its controller stamps, so before that it has
	// nothing to bind to; the pod kind composes the target from the handle
	// alone and is always able to bind.
	ExecutorNeedsRunningSandbox bool
	// PrewarmPoolRequest returns a fresh PoolRequest for a Prewarmer backend
	// under test. A func, not a value, for the same reason as NewRequest: pool
	// identity is commonly derived from ClassName alone and stays stable across
	// class edits by design, so reusing one ClassName would collide subtests on
	// a single underlying pool. Required whenever Runtime type-asserts to
	// Prewarmer; nil otherwise (pod does not implement it).
	PrewarmPoolRequest func() sandboxkinds.PoolRequest
	// CountPrewarmPoolObjects reports how many backend-native objects still
	// represent className's pre-warmed pool in namespace -- the signal
	// SweepOrphanedPools promises to drive to zero for a namespace it
	// reclaims. The seam has no generic pool-observation verb
	// (Handle/Status/Ensure/Teardown all address one SANDBOX), so this is how a
	// backend lets the shared suite watch its native objects without this
	// package importing that backend's types.
	//
	// REQUIRED for a Prewarmer backend, not optional: every other row in the
	// section asserts a returned error only, so a `return nil` stub satisfies
	// them, and this is what the one row asserting a real RECLAIM depends on. A
	// contract a stub can pass is not a contract.
	CountPrewarmPoolObjects func(ctx context.Context, namespace, className string) (int, error)
}

// Run executes the full contract against s.
func Run(t *testing.T, s Subject) {
	t.Helper()

	t.Run(s.Name+"/Ensure returns a handle owned by the backend", func(t *testing.T) {
		h, err := s.Runtime.Ensure(context.Background(), s.NewRequest())
		require.NoError(t, err)
		assert.NotEmpty(t, h.Kind, "a handle must name its owning kind")
		assert.NotEmpty(t, h.Ref, "a handle must carry a backend reference")
	})

	// The status write that persists a handle can fail AFTER Ensure succeeds.
	// The next reconcile then re-enters with no handle and must converge on the
	// same sandbox rather than creating a second one.
	t.Run(s.Name+"/Ensure is idempotent and converges on one sandbox", func(t *testing.T) {
		req := s.NewRequest()
		first, err := s.Runtime.Ensure(context.Background(), req)
		require.NoError(t, err)

		second, err := s.Runtime.Ensure(context.Background(), req)
		require.NoError(t, err, "a second Ensure must not conflict")
		assert.Equal(t, first, second, "a second Ensure must return the same handle")
	})

	t.Run(s.Name+"/Status reports a known phase", func(t *testing.T) {
		h, err := s.Runtime.Ensure(context.Background(), s.NewRequest())
		require.NoError(t, err)

		got, err := s.Runtime.Status(context.Background(), h)
		require.NoError(t, err)
		assert.Contains(t, []sandboxkinds.Phase{
			sandboxkinds.PhasePending, sandboxkinds.PhaseReady,
			sandboxkinds.PhaseFailed, sandboxkinds.PhaseGone,
		}, got.Phase, "Status must map onto AP's phase vocabulary")

		if s.ExpectReadyAfterEnsure {
			assert.Equal(t, sandboxkinds.PhaseReady, got.Phase)
		}
	})

	t.Run(s.Name+"/Status of a foreign handle is refused", func(t *testing.T) {
		_, err := s.Runtime.Status(context.Background(),
			sandboxkinds.Handle{Kind: "definitely-not-this-kind", Ref: "x"})
		assert.Error(t, err, "a handle owned by another kind must be refused, not interpreted")
	})

	// A completed or reaped session re-enters Status on every reconcile. Gone
	// must be a phase, not an error, or the logs fill with false alarms.
	t.Run(s.Name+"/Status after Teardown is Gone, not an error", func(t *testing.T) {
		h, err := s.Runtime.Ensure(context.Background(), s.NewRequest())
		require.NoError(t, err)
		require.NoError(t, s.Runtime.Teardown(context.Background(), h))

		got, err := s.Runtime.Status(context.Background(), h)
		require.NoError(t, err, "a torn-down sandbox must not make Status error")
		assert.Equal(t, sandboxkinds.PhaseGone, got.Phase)
	})

	t.Run(s.Name+"/Teardown is idempotent", func(t *testing.T) {
		h, err := s.Runtime.Ensure(context.Background(), s.NewRequest())
		require.NoError(t, err)

		require.NoError(t, s.Runtime.Teardown(context.Background(), h))
		assert.NoError(t, s.Runtime.Teardown(context.Background(), h),
			"tearing down an already-gone sandbox must succeed")
	})

	// Teardown is the DESTRUCTIVE verb, so its foreign-handle rejection matters
	// most: a ResolveHandle regression here deletes another backend's object
	// rather than failing a tool call. Two kinds can name sandboxes the same
	// way (both shipped kinds use podspec.PodNameFor), and Teardown treats an
	// already-gone sandbox as success, so the deletion leaves no error behind.
	t.Run(s.Name+"/Teardown refuses a foreign handle", func(t *testing.T) {
		err := s.Runtime.Teardown(context.Background(),
			sandboxkinds.Handle{Kind: "definitely-not-this-kind", Ref: "x"})
		assert.Error(t, err, "a handle owned by another kind must be refused, not deleted")
	})

	t.Run(s.Name+"/Executor binds to the handle", func(t *testing.T) {
		h, err := s.Runtime.Ensure(context.Background(), s.NewRequest())
		require.NoError(t, err)

		ex, err := s.Runtime.Executor(h)
		if s.ExecutorNeedsRunningSandbox {
			// The sandbox is not running here, so the contract is that the
			// backend REFUSES. Binding to an unresolved target would dispatch a
			// tool call at nothing and fail somewhere far from the real cause.
			require.Error(t, err, "a backend that cannot resolve its exec target must refuse, not bind to nothing")
			assert.Nil(t, ex, "a backend that refuses must not also return an executor")
			return
		}
		require.NoError(t, err)
		require.NotNil(t, ex, "a backend must return a usable executor for a live handle")
	})

	t.Run(s.Name+"/Executor refuses a foreign handle", func(t *testing.T) {
		_, err := s.Runtime.Executor(
			sandboxkinds.Handle{Kind: "definitely-not-this-kind", Ref: "x"})
		assert.Error(t, err)
	})

	// Watches may legitimately be empty — a backend outside the cluster has
	// nothing cluster-local to observe — but every entry must carry an object.
	t.Run(s.Name+"/Watches entries are well-formed", func(t *testing.T) {
		for i, w := range s.Runtime.Watches() {
			assert.NotNil(t, w.Object, "Watches()[%d] must carry an object", i)
		}
	})

	// Prewarmer is optional, decided by the type assertion rather than a
	// parallel Feature flag, so a backend that does not implement it skips this
	// section instead of failing it.
	//
	// The idempotence/no-op rows assert only what the returned error promises.
	// They deliberately do not count backend-native objects: "exactly one
	// template" is a fact about one implementation, not the contract, and
	// belongs in that backend's own tests.
	//
	// TWO rows are different in kind, and they are what stops a `return nil`
	// stub from passing the whole section:
	//
	//   - The empty-ClassUID row asserts a REFUSAL — a seam-level MUST (see
	//     PoolRequest.ClassUID) assertable with no backend types in sight.
	//   - The reclaim row asserts an actual RECLAIM: a namespace dropped from
	//     the desired set must stop being represented, which is only provable
	//     by counting, hence CountPrewarmPoolObjects.
	if pw, ok := s.Runtime.(sandboxkinds.Prewarmer); ok {
		require.NotNil(t, s.PrewarmPoolRequest,
			"a Prewarmer backend must supply PrewarmPoolRequest; the rows below have no "+
				"generic way to construct a pool request for a backend they know nothing about")
		require.NotNil(t, s.CountPrewarmPoolObjects,
			"a Prewarmer backend must supply CountPrewarmPoolObjects: without it every "+
				"remaining row asserts a returned error only, and a do-nothing Prewarmer "+
				"would pass the whole section")

		t.Run(s.Name+"/ReconcilePool refuses an empty ClassUID rather than creating an unowned pool", func(t *testing.T) {
			// PoolRequest.ClassUID is what a backend stamps as the pooled
			// objects' ownerReference. Creating the pool anyway when it is
			// absent leaves objects nothing will ever reclaim — the exact leak
			// the ownerReference exists to prevent — and, unlike a wrong
			// ownerReference, it is invisible until the class is deleted.
			req := s.PrewarmPoolRequest()
			req.ClassUID = ""
			assert.Error(t, pw.ReconcilePool(context.Background(), req),
				"an empty ClassUID must fail closed, never create an unowned pool")
		})

		t.Run(s.Name+"/ReconcilePool is idempotent: a second identical call succeeds", func(t *testing.T) {
			req := s.PrewarmPoolRequest()
			require.NoError(t, pw.ReconcilePool(context.Background(), req))
			// ReconcilePool runs on EVERY class reconcile, so a second call with
			// the identical request must succeed exactly as the first did rather
			// than conflict with the capacity the first call already brought up.
			assert.NoError(t, pw.ReconcilePool(context.Background(), req))
		})

		t.Run(s.Name+"/ReconcilePool accepts Replicas 0, and tearing an already-torn-down pool down again is still a no-op", func(t *testing.T) {
			req := s.PrewarmPoolRequest()
			require.NoError(t, pw.ReconcilePool(context.Background(), req))

			req.Replicas = 0
			require.NoError(t, pw.ReconcilePool(context.Background(), req), "Replicas: 0 must be accepted, not refused")
			// A class with no warmPool request (or one that just turned
			// pre-warming off) reconciles every pass, so a second Replicas: 0
			// call over an already-torn-down pool must not become a reconcile
			// error.
			assert.NoError(t, pw.ReconcilePool(context.Background(), req))
		})

		t.Run(s.Name+"/SweepOrphanedPools is idempotent and a no-op when the pool's namespace is still desired", func(t *testing.T) {
			req := s.PrewarmPoolRequest()
			require.NoError(t, pw.ReconcilePool(context.Background(), req))

			// req.Namespace is still in the desired set, so nothing here is
			// orphaned — the common case on every class reconcile per
			// SweepOrphanedPools's own doc comment.
			require.NoError(t, pw.SweepOrphanedPools(context.Background(), req.ClassName, req.ClassUID, []string{req.Namespace}))
			assert.NoError(t, pw.SweepOrphanedPools(context.Background(), req.ClassName, req.ClassUID, []string{req.Namespace}),
				"a second sweep over an unchanged desired set must not error")
		})

		t.Run(s.Name+"/SweepOrphanedPools accepts an empty desired set, and the class can reconcile its pool again afterward", func(t *testing.T) {
			req := s.PrewarmPoolRequest()
			require.NoError(t, pw.ReconcilePool(context.Background(), req))

			// Empty desiredNamespaces means "reclaim every pool this class has,
			// in every namespace" per SweepOrphanedPools's own doc comment — the
			// shape a fully-cleared warmPool stanza resolves to.
			require.NoError(t, pw.SweepOrphanedPools(context.Background(), req.ClassName, req.ClassUID, nil))

			// A class that re-opts into pre-warming after a full sweep must be
			// able to bring its pool back up cleanly, not trip over anything the
			// sweep left behind.
			assert.NoError(t, pw.ReconcilePool(context.Background(), req))
		})

		t.Run(s.Name+"/SweepOrphanedPools observably reclaims a namespace dropped from the desired set, and leaves one still desired untouched", func(t *testing.T) {
			req := s.PrewarmPoolRequest()
			require.NoError(t, pw.ReconcilePool(context.Background(), req))

			before, err := s.CountPrewarmPoolObjects(context.Background(), req.Namespace, req.ClassName)
			require.NoError(t, err)
			require.Positive(t, before, "precondition: ReconcilePool must have created at least one observable pool object")

			// req.Namespace is still named in desiredNamespaces, so the sweep
			// must leave it exactly as it found it.
			require.NoError(t, pw.SweepOrphanedPools(context.Background(), req.ClassName, req.ClassUID, []string{req.Namespace}))
			stillDesired, err := s.CountPrewarmPoolObjects(context.Background(), req.Namespace, req.ClassName)
			require.NoError(t, err)
			assert.Equal(t, before, stillDesired, "a namespace still in the desired set must be untouched by the sweep")

			// req.Namespace is no longer named anywhere -- the "reclaim
			// everything" shape an empty desiredNamespaces means -- so the
			// count must fall to zero, not merely return a nil error.
			require.NoError(t, pw.SweepOrphanedPools(context.Background(), req.ClassName, req.ClassUID, nil))
			dropped, err := s.CountPrewarmPoolObjects(context.Background(), req.Namespace, req.ClassName)
			require.NoError(t, err)
			assert.Zero(t, dropped, "a namespace dropped from the desired set must be reclaimed, not merely reported as swept without error")
		})
	}
}
