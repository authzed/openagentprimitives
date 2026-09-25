// Package idempotency provides two CI-gate checks for the merge/apply
// convention in AGENTS.md's "Server-side apply: keep applied fields idempotent;
// put observations in status":
//
//   - CheckApplyIdempotent: a byte-identical re-apply of an SSA payload must be
//     a true no-op, with no resourceVersion bump. Catches a volatile value —
//     time.Now(), a random ID, a recomputed digest — leaking into a field a
//     client server-side-applies.
//   - CheckReconcileConverges: a reconciler must stop rewriting an object once
//     its desired state is reached. Catches a controller that unconditionally
//     restamps an observed-at or status field on every pass, producing a
//     reconcile storm.
//
// Each guards a real production bug: `oap agent install` SSA-applying an
// `oap-source` annotation embedding `installedAt: time.Now()`, fixed by moving
// the timestamp to controller-owned status; and the guardian AgentSessionGrants
// controller stamping a fresh ObservedSchemaWrittenAt every reconcile rather
// than only on the False→True transition, a ~5s-forever storm.
//
// Each check splits into an error-returning core (Check*) and a require-based
// wrapper (Require*), so this package's OWN tests can assert the core DETECTS a
// violation — something a t.FailNow-calling helper could never do.
//
// No build tag and no envtest import; the checks need only a client.Client and,
// for CheckReconcileConverges, a reconcile.Reconciler. Both are nonetheless
// meaningful ONLY against a real apiserver — see each function's doc.
package idempotency

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// CheckApplyIdempotent SSA-applies want TWICE under fieldManager and errors if
// the second apply mutated the live object relative to the first, i.e. bumped
// resourceVersion — the signature of a volatile value leaking into an applied
// field.
//
// want is DeepCopy'd before EACH apply. An SSA Patch overwrites the pointer it
// is given with the server's response, so reusing it for the second apply would
// re-apply what the FIRST apply produced, never testing whether a fresh
// construction of "the same" desired state actually diverges.
//
// Any resourceVersion already on want is cleared before each apply:
// resourceVersion is never a field an apply payload should carry, and a stale
// one would raise a conflict unrelated to the question being asked.
//
// MUST run against a REAL apiserver. The fake client bumps resourceVersion on
// every accepted Patch instead of reproducing the apiserver's byte-equality
// no-op, so against it this check passes vacuously.
func CheckApplyIdempotent(ctx context.Context, c client.Client, want client.Object, fieldManager string) error {
	apply := func(step int) (client.Object, error) {
		obj, ok := want.DeepCopyObject().(client.Object)
		if !ok {
			return nil, fmt.Errorf("idempotency: CheckApplyIdempotent: want.DeepCopyObject() returned %T, not a client.Object", obj)
		}
		obj.SetResourceVersion("")
		if err := c.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
			return nil, fmt.Errorf("idempotency: CheckApplyIdempotent: apply #%d: %w", step, err)
		}

		got, ok := want.DeepCopyObject().(client.Object)
		if !ok {
			return nil, fmt.Errorf("idempotency: CheckApplyIdempotent: want.DeepCopyObject() returned %T, not a client.Object", got)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(want), got); err != nil {
			return nil, fmt.Errorf("idempotency: CheckApplyIdempotent: get after apply #%d: %w", step, err)
		}
		return got, nil
	}

	first, err := apply(1)
	if err != nil {
		return err
	}
	second, err := apply(2)
	if err != nil {
		return err
	}

	if first.GetResourceVersion() == second.GetResourceVersion() {
		return nil
	}

	msg := fmt.Sprintf(
		"idempotency: apply is NOT idempotent for %s (fieldManager=%q): resourceVersion changed %s -> %s across two applies of the identical payload — a volatile value (time.Now(), a random ID, a recomputed digest) is likely leaking into an applied field; move it to controller-owned status instead (see AGENTS.md \"Server-side apply: keep applied fields idempotent; put observations in status\")",
		client.ObjectKeyFromObject(first), fieldManager, first.GetResourceVersion(), second.GetResourceVersion(),
	)
	if d, derr := diffObjects(first, second); derr == nil && d != "" {
		msg += fmt.Sprintf("\ndiff between the two applied objects (-first, +second):\n%s", d)
	}
	return errors.New(msg)
}

// RequireApplyIdempotent is the require-based wrapper over
// CheckApplyIdempotent. See that function for the full contract.
func RequireApplyIdempotent(t *testing.T, ctx context.Context, c client.Client, want client.Object, fieldManager string) {
	t.Helper()
	require.NoError(t, CheckApplyIdempotent(ctx, c, want, fieldManager))
}

// CheckReconcileConverges reconciles req against r up to maxSteps times, Get'ing
// obj's namespaced name after each pass, and returns nil the moment TWO
// CONSECUTIVE passes leave resourceVersion unchanged — steady state, nothing
// left to write. It errors if the reconciler never stabilizes: a reconcile
// storm, where the controller rewrites the object on every pass, typically by
// restamping a timestamp unconditionally rather than on a real transition.
//
// obj is a prototype only, DeepCopy'd before each Get so the result matches
// whatever type the caller passed; its contents are ignored.
//
// maxSteps must be >= 2, since two consecutive passes cannot be observed in
// fewer than two Reconcile calls.
//
// MUST run against a REAL apiserver, for the same reason as
// CheckApplyIdempotent: convergence means the status Patch computed an empty
// diff and the apiserver skipped the write, which only etcd3's
// no-op detection reproduces. The fake client bumps resourceVersion on every
// accepted Patch, so it reports "never converges" even for a correct controller.
func CheckReconcileConverges(ctx context.Context, c client.Client, r reconcile.Reconciler, req reconcile.Request, obj client.Object, maxSteps int) error {
	if maxSteps < 2 {
		return fmt.Errorf("idempotency: CheckReconcileConverges: maxSteps must be >= 2 (two consecutive passes are needed to observe convergence), got %d", maxSteps)
	}

	rvHistory := make([]string, 0, maxSteps)
	var secondToLast, last client.Object

	for step := 1; step <= maxSteps; step++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			return fmt.Errorf("idempotency: CheckReconcileConverges: reconcile step %d/%d for %s errored: %w", step, maxSteps, req.NamespacedName, err)
		}

		current, ok := obj.DeepCopyObject().(client.Object)
		if !ok {
			return fmt.Errorf("idempotency: CheckReconcileConverges: obj.DeepCopyObject() returned %T, not a client.Object", current)
		}
		if err := c.Get(ctx, req.NamespacedName, current); err != nil {
			return fmt.Errorf("idempotency: CheckReconcileConverges: get after reconcile step %d/%d for %s: %w", step, maxSteps, req.NamespacedName, err)
		}
		rvHistory = append(rvHistory, current.GetResourceVersion())

		if last != nil && current.GetResourceVersion() == last.GetResourceVersion() {
			return nil // two consecutive passes left resourceVersion unchanged: converged
		}
		secondToLast, last = last, current
	}

	msg := fmt.Sprintf(
		"idempotency: reconcile did NOT converge within %d step(s) for %s (resourceVersion history: %v) — the reconciler appears to rewrite the object on every pass (a reconcile storm: look for a value stamped unconditionally, e.g. time.Now(), rather than only on a meaningful state transition; see AGENTS.md \"Server-side apply: keep applied fields idempotent; put observations in status\")",
		maxSteps, req.NamespacedName, rvHistory,
	)
	if secondToLast != nil && last != nil {
		if d, derr := diffObjects(secondToLast, last); derr == nil && d != "" {
			msg += fmt.Sprintf("\ndiff between the last two observed objects (-earlier, +later):\n%s", d)
		}
	}
	return errors.New(msg)
}

// RequireReconcileConverges is the require-based wrapper over
// CheckReconcileConverges for production tests. See
// CheckReconcileConverges's doc comment for the full contract (real
// apiserver required, maxSteps >= 2, etc).
func RequireReconcileConverges(t *testing.T, ctx context.Context, c client.Client, r reconcile.Reconciler, req reconcile.Request, obj client.Object, maxSteps int) {
	t.Helper()
	require.NoError(t, CheckReconcileConverges(ctx, c, r, req, obj, maxSteps))
}

// diffObjects renders a best-effort structural diff between two
// client.Object values for error messages — naming which annotation,
// label, or spec field differs rather than leaving the next engineer to
// bisect a bare resourceVersion mismatch. Volatile system bookkeeping
// fields that legitimately differ on every write (resourceVersion,
// generation, managedFields, creationTimestamp, uid, selfLink) are
// stripped first so they never drown out the actual offending field.
func diffObjects(a, b client.Object) (string, error) {
	am, err := runtime.DefaultUnstructuredConverter.ToUnstructured(a)
	if err != nil {
		return "", fmt.Errorf("convert first object to unstructured: %w", err)
	}
	bm, err := runtime.DefaultUnstructuredConverter.ToUnstructured(b)
	if err != nil {
		return "", fmt.Errorf("convert second object to unstructured: %w", err)
	}
	// ToUnstructured aliases the live map for an *unstructured.Unstructured
	// input (UnstructuredContent returns u.Object itself, not a copy) — deep
	// copy before mutating so we never corrupt the caller's object.
	am = runtime.DeepCopyJSON(am)
	bm = runtime.DeepCopyJSON(bm)
	stripVolatileMeta(am)
	stripVolatileMeta(bm)
	return cmp.Diff(am, bm), nil
}

// stripVolatileMeta deletes the metadata bookkeeping fields the apiserver
// itself manages and which legitimately change on every accepted write, so
// diffObjects's output stays focused on the field a caller actually applied
// or a controller actually wrote.
func stripVolatileMeta(m map[string]interface{}) {
	meta, ok := m["metadata"].(map[string]interface{})
	if !ok {
		return
	}
	for _, k := range []string{"resourceVersion", "generation", "managedFields", "creationTimestamp", "uid", "selfLink"} {
		delete(meta, k)
	}
}
