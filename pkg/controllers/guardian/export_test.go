package guardian

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// MCPServerToAllASGsForTest exposes the unexported mcpServerToAllASGs
// mapper to the guardian_test package so it can be unit-tested
// independently of controller-runtime's manager wiring. Test-only.
func MCPServerToAllASGsForTest(r *Reconciler, ctx context.Context, obj client.Object) []reconcile.Request {
	return r.mcpServerToAllASGs(ctx, obj)
}

// SetDebounceForTest overrides the reconciler's write debounce window
// (default 5s) so tests that drive multiple consecutive Reconcile calls
// can observe the post-debounce I/O path each time. Pass 0 to disable
// debounce entirely. Test-only.
func SetDebounceForTest(r *Reconciler, d time.Duration) {
	r.debounce = d
}

// BadSpiceboxToolkitForTest is the guardian_test-visible stand-in for the
// unexported invalidSpiceboxToolkitFragment: same three facts (which
// toolkit, why, the underlying error), exported so a test outside this
// package can build one. Test-only.
type BadSpiceboxToolkitForTest struct {
	Toolkit *spiceboxv1alpha1.SpiceboxToolkit
	Reason  string
	Err     error
}

// PatchSpiceboxToolkitSchemaValidityForTest exposes the unexported
// patchSpiceboxToolkitSchemaValidity to the guardian_test package so it can
// be unit-tested independently of the full Reconcile flow. Test-only.
func PatchSpiceboxToolkitSchemaValidityForTest(r *Reconciler, ctx context.Context, all []spiceboxv1alpha1.SpiceboxToolkit, bad []BadSpiceboxToolkitForTest) {
	internal := make([]invalidSpiceboxToolkitFragment, len(bad))
	for i, b := range bad {
		internal[i] = invalidSpiceboxToolkitFragment{toolkit: b.Toolkit, reason: b.Reason, err: b.Err}
	}
	r.patchSpiceboxToolkitSchemaValidity(ctx, all, internal)
}

// DetectBootstrapDriftForTest exposes the unexported detectBootstrapDrift
// method to the guardian_test package, with no tuple exempted from Missing
// (alreadyKnown=nil) — matching this method's behaviour before the
// first-reconcile false-positive fix, which every existing caller of this
// helper depends on. Test-only — production code calls r.detectBootstrapDrift
// directly from Reconcile (agentsessiongrants_controller.go), passing
// r.lastDesired as alreadyKnown.
func DetectBootstrapDriftForTest(r *Reconciler, ctx context.Context, desired *DesiredMap) driftReport {
	return r.detectBootstrapDrift(ctx, desired, nil)
}

// DetectBootstrapDriftWithKnownForTest is DetectBootstrapDriftForTest plus an
// explicit alreadyKnown set: a desired tuple absent from alreadyKnown is
// treated as freshly claimed (never converged before) and is exempt from
// Missing — see detectBootstrapDrift's doc comment. Test-only.
func DetectBootstrapDriftWithKnownForTest(r *Reconciler, ctx context.Context, desired, alreadyKnown *DesiredMap) driftReport {
	return r.detectBootstrapDrift(ctx, desired, alreadyKnown)
}

// SetClockForTest overrides the reconciler's drift-event clock (nil default:
// time.Now). Test-only seam for pinning a monitoring event's Timestamp to an
// exact, assertable value.
func SetClockForTest(r *Reconciler, clock func() time.Time) {
	r.clock = clock
}

// RelationshipOwnershipRefusalsForTest exposes the unexported
// relationshipOwnershipRefusals to the guardian_test package — the actual
// per-relationship ownership check bootstrap_sync.go's validateBootstraps
// calls in production, and what TestValidation_AgreesWithTheWriteTimeGuard
// must agree with (ValidateSpec, in bootstrap_validation.go, is confirmed
// production-dead and calling it would only prove agreement with itself).
// Test-only.
func RelationshipOwnershipRefusalsForTest(boot *spiceboxv1alpha1.SpiceDBBootstrap) []string {
	return relationshipOwnershipRefusals(boot)
}
