package sandboxkinds_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every Runtime method must have a real consumer. Runtime.Teardown once
// shipped with none: the session finalizer still deleted a pod directly, so
// any non-pod backend would have leaked its sandbox on every session delete,
// silently and permanently. Per-task review missed it because each task was
// correct in isolation and the gap was in the join between them.
//
// This is a curated table rather than a call-graph scan on purpose. Method
// names like "Status" collide with client.Status() throughout the repo, so a
// name-only search would match something unrelated and pass while proving
// nothing. Requiring an explicit consumer package makes an author adding a
// seam method decide where it is used.
//
// Needles are receiver-qualified ("rt.Status(", not ".Status(") because a
// bare ".Status(" is not actually selective: pkg/controllers/spiceboxsession
// contains four unrelated matches —
// controller.go:222, :253, :297 (`r.Client.Status().Update(ctx, &sess)`) and
// ttl.go:83 (`r.Client.Status().Patch(ctx, &fresh, patch)`) — alongside the
// one genuine seam call, controller.go:418 (`rt.Status(ctx, h)`). A bare
// needle would keep passing even if the real call at :418 were deleted,
// which is exactly the vacuous-assertion failure mode this test exists to
// prevent, and Status is the one seam verb with a same-named stdlib-ish
// neighbour. Yes, this couples the test to the local variable name `rt` used
// at every genuine call site (controller.go:74, :189, :381, :418;
// toolcall/executor.go:26) — that coupling is intentional: renaming the
// variable makes this test fail loudly, forcing a human to look at the call
// site, instead of the substring scan silently staying green.
func TestSeamCoverage_EveryRuntimeMethodHasAConsumer(t *testing.T) {
	cases := []struct {
		method   string
		consumer string // repo-relative dir whose non-test files must call it
	}{
		{method: "rt.Ensure(", consumer: "pkg/controllers/spiceboxsession"},
		{method: "rt.Status(", consumer: "pkg/controllers/spiceboxsession"},
		{method: "rt.Teardown(", consumer: "pkg/controllers/spiceboxsession"},
		{method: "rt.Executor(", consumer: "pkg/controllers/toolcall"},
		{method: "rt.Watches(", consumer: "pkg/controllers/spiceboxsession"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" is called from "+tc.consumer, func(t *testing.T) {
			assert.True(t, dirCalls(t, tc.consumer, tc.method),
				"no non-test file under %s calls %s — a seam method with no consumer "+
					"is dead code at best and a silent resource leak at worst",
				tc.consumer, tc.method)
		})
	}
}

// Every Kind method must have a real consumer too, for the same reason and
// with the same needle discipline as the Runtime table above.
//
// This table exists because guarding only Runtime is what let two Kind methods
// ship unwired. Kind.ValidateClass documented itself as joining the
// spiceboxclass controller's validation chain and had zero non-test callers:
// the controller called the free function ValidateClassAgainstKind instead, so
// a bring-your-own backend implementing ValidateClass per the interface
// contract was silently ignored — the class went Valid=True and the backend
// rendered a sandbox missing whatever it meant to reject. Kind.WorkspaceDomain
// reached only an unwired helper (see the test below). Neither was findable by
// reading either half in isolation; both are one grep away with a table.
//
// A Kind method whose only caller is inside pkg/tools/sandboxkinds is legitimate
// when the seam's OWN exported helper is the consumer and that helper has its
// own consumers — Supports is reached from both controllers through
// ValidateClassAgainstKind. The row names the package that actually contains
// the call, not the package that ultimately benefits, so the needle stays
// checkable.
func TestSeamCoverage_EveryKindMethodHasAConsumer(t *testing.T) {
	cases := []struct {
		method   string
		consumer string // repo-relative dir whose non-test files must call it
	}{
		// The operator names every registered kind while constructing its
		// runtime, and keys the Runtimes map by it.
		{method: "k.Name(", consumer: "internal/cmd/operator"},
		// Reached from both controllers through ValidateClassAgainstKind, which
		// takes the Kind as a parameter so this package never imports its own
		// registry.
		{method: "k.Supports(", consumer: "pkg/tools/sandboxkinds"},
		// Class-time validation, and again at session bind after the tier
		// override may have replaced the kind the class declared.
		{method: "sbKind.ValidateClass(", consumer: "pkg/controllers/spiceboxclass"},
		{method: "sbKind.ValidateClass(", consumer: "pkg/controllers/spiceboxsession"},
		// One runtime per registered kind, built once at operator startup.
		{method: "k.NewRuntime(", consumer: "internal/cmd/operator"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" is called from "+tc.consumer, func(t *testing.T) {
			assert.True(t, dirCalls(t, tc.consumer, tc.method),
				"no non-test file under %s calls %s — a seam method with no consumer "+
					"is a contract the interface advertises and nothing honours",
				tc.consumer, tc.method)
		})
	}
}

// Prewarmer is the first *optional* interface (alongside Snapshotter,
// Suspender, FileTransferer, none of which has a row here yet) to get a
// seam-coverage guard. An optional interface with a real implementation and
// zero verified callers is the same "seam surface that outran its consumers"
// defect this file exists to catch — Runtime.Teardown and Kind.ValidateClass
// both shipped that way — so Prewarmer's consumer is asserted starting from
// the same commit that adds the interface, rather than left to be noticed
// later.
//
// ReconcilePool's row was RED from the interface's introduction until it was
// wired into pkg/controllers/spiceboxclass — that was the guard doing its job,
// proving the interface would actually be consumed rather than asserting it
// already was. SweepOrphanedPools was added to the interface later, in the
// same review round that closed the pool-orphaning defect (a namespace
// removed from a class's warmPool.namespaces, or the whole stanza cleared,
// left ReconcilePool's own per-namespace loop with nothing to revisit); its
// row follows the identical pattern.
func TestSeamCoverage_EveryPrewarmerMethodHasAConsumer(t *testing.T) {
	cases := []struct {
		method   string
		consumer string // repo-relative dir whose non-test files must call it
	}{
		// Deliberately UNqualified, unlike the rt./sbKind. rows above: those are
		// qualified because ".Status(" and similar collide with unrelated calls
		// in the same consumer directory (see the file doc comment), so an
		// unqualified needle would stay green after the real call site was
		// deleted. Both ReconcilePool and SweepOrphanedPools are identifiers
		// that appear nowhere in the repo outside this package, so qualifying
		// them buys zero selectivity today — and it would cost real precision:
		// the call site type-asserts into a locally-named variable this test
		// does not control. The leading "." is kept so a same-named
		// package-level function would still be excluded.
		{method: ".ReconcilePool(", consumer: "pkg/controllers/spiceboxclass"},
		{method: ".SweepOrphanedPools(", consumer: "pkg/controllers/spiceboxclass"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" is called from "+tc.consumer, func(t *testing.T) {
			assert.True(t, dirCalls(t, tc.consumer, tc.method),
				"no non-test file under %s calls %s — a seam method with no consumer "+
					"is dead code at best and a silent resource leak at worst",
				tc.consumer, tc.method)
		})
	}
}

// Status.RequiresPolling is the first seam FIELD to get a coverage guard. The
// two tables above walk methods, and a field is not a method — so rather than
// contorting them (a `.RequiresPolling` row under a test named
// "EveryRuntimeMethodHasAConsumer" would be a lie about what it checks), it
// gets its own test using the same dirCalls needle discipline.
//
// It needs BOTH halves, which is what makes it different from a method row. A
// method with no consumer is dead code; a Status field can fail in two
// directions:
//
//   - PRODUCED by nobody — the controller polls on a flag no backend ever sets,
//     so the liveness fix silently does nothing and an adopted session stalls
//     until the manager's ~10h resync.
//   - CONSUMED by nobody — every backend faithfully reports "poll me" into a
//     void. That is this branch's signature defect, seam surface outrunning its
//     consumers, and it is exactly what a field (unlike a method) can do
//     without any compiler complaint at all.
//
// The producer needle is unqualified with a trailing colon (`RequiresPolling:`)
// because a struct-literal field is what production code writes; the consumer
// needle is receiver-qualified (`st.RequiresPolling`) in the same style, and
// for the same reason, as the `rt.Status(` rows above.
func TestSeamCoverage_StatusRequiresPollingIsBothProducedAndConsumed(t *testing.T) {
	assert.True(t, dirCalls(t, "pkg/tools/sandboxkinds/agentsandbox", "RequiresPolling:"),
		"no non-test file under pkg/tools/sandboxkinds/agentsandbox SETS Status.RequiresPolling — "+
			"a field the consumer branches on that no backend ever produces makes the "+
			"session controller's requeue silently dead, and an adopted session stalls "+
			"until the manager's ~10h resync")

	assert.True(t, dirCalls(t, "pkg/controllers/spiceboxsession", "st.RequiresPolling"),
		"no non-test file under pkg/controllers/spiceboxsession READS Status.RequiresPolling — "+
			"a seam field with no consumer is a contract the interface advertises and "+
			"nothing honours, and every backend reporting it would be reporting into a void")
}

// Kind.WorkspaceDomain is the one seam method with no consumer outside this
// package: it is reached only by CheckWorkspaceDomains, which is itself unwired
// (see its doc comment). This test PINS that gap rather
// than papering over it — a row in the table above claiming a controller
// consumer would be a lie, and one claiming pkg/tools/sandboxkinds would pass while
// proving nothing.
//
// When CheckWorkspaceDomains is wired, this test fails. That is the point:
// deleting it and adding the real row to the table above is part of wiring it.
func TestSeamCoverage_CheckWorkspaceDomainsIsStillUnwired(t *testing.T) {
	require.True(t, dirCalls(t, "pkg/tools/sandboxkinds", "k.WorkspaceDomain("),
		"CheckWorkspaceDomains must still be WorkspaceDomain's caller")

	assert.False(t, dirCalls(t, "pkg/controllers/agentsession", "CheckWorkspaceDomains("),
		"CheckWorkspaceDomains is now wired into the AgentSession reconciler — "+
			"delete this test, add a WorkspaceDomain row naming that consumer to "+
			"TestSeamCoverage_EveryKindMethodHasAConsumer, and drop the NOT YET WIRED "+
			"note from its doc comment")
}

// Negative control: dirCalls must be able to return false. Without this, a
// helper that always returned true would make every row above pass and the
// entire test meaningless.
func TestSeamCoverage_DirCallsCanFail(t *testing.T) {
	assert.False(t, dirCalls(t, "pkg/controllers/spiceboxsession", "rt.NoSuchSeamMethod("),
		"dirCalls must report false for a method that is not called")
}

// dirCalls reports whether any non-test .go file directly under dir contains
// needle. Non-recursive: the consumer is named exactly, so a match in a
// subpackage would be a different consumer than the table claims.
func dirCalls(t *testing.T, dir, needle string) bool {
	t.Helper()
	root := filepath.Join("..", "..", "..", dir)
	entries, err := os.ReadDir(root)
	require.NoError(t, err, "read consumer dir %s", root)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name()))
		require.NoError(t, err, "read %s", e.Name())
		if strings.Contains(string(b), needle) {
			return true
		}
	}
	return false
}
