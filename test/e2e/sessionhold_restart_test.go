//go:build e2e

// The forensic-hold design's central claim is that the ap.revocation bus is a
// latency-only fast path and the SessionHold CR (via the AgentSession
// reconciler's early-return park in reconcileHold) is the actual guarantee.
// These tests exercise that claim directly rather than trusting the prose: a held session
// must stay held across an operator restart and across the runner going away
// and coming back, and a trip whose bus envelope is never published must
// still reach Held. If any of these needed the bus, or needed the SAME
// operator process that tripped it, the design's own justification for
// rejecting a bus-only alternative would be wrong.
package e2e_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/nats/subjects"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// sessionHoldFixture boots the centerdot fixture with NO plan-gate
// enforcement — these tests are about hold PERSISTENCE once tripped, not
// about the plan-gate denial-streak tripper (covered end to end by the
// bronzethread bundle testdata/plangate-denial-streak-hold) — and drives one
// trivial user turn so a real AgentSession materializes, resolved and live,
// before a hold is placed on it.
func sessionHoldFixture(t *testing.T) (h *e2e.Harness, ns, name string) {
	t.Helper()
	h = e2e.Start(t, e2e.Options{AgentDir: "testdata/agent-centerdot-companies"})

	// Every tool the fixture's MCPServer declares needs a handler registered
	// before class admission, even one this test's own script never calls —
	// the allowlist is validated at class-admission time, and a missing
	// handler surfaces as AgentClassMCPServerInvalid/AllowlistDrift rather
	// than as anything mentioning the tool (see planGateHarness in
	// plangate_test.go, same fixture).
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})

	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	h.LLM.OnUserMessage("hello").Reply(e2e.RespondToUser("hello yourself"))
	// Trailing catch-all: the runner's post-respond_to_user epilogue probe
	// (see bronzethread driver_test.go's identical rule) is not part of the
	// authored exchange above; without this any such probe fails with "no
	// rule matched".
	h.LLM.On(func(llm.Request) bool { return true }).Reply(e2e.EndTurn()).Repeating()

	h.SendUserMessage("hello")
	h.ExpectAgentReply(e2e.Contains("hello yourself"))

	ns, name = h.SessionRef()
	return h, ns, name
}

// createManualHold creates a SessionHold CR the same shape `oap session
// hold` does (spec.source="manual") — the manual half of the design's "one
// path, two authors." Persistence properties should not care which author
// tripped the hold; using the manual path here keeps these tests independent
// of the plan-gate tripper entirely.
func createManualHold(t *testing.T, h *e2e.Harness, ns, name, holdName string) {
	t.Helper()
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: holdName, Namespace: ns},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: name},
			Reason:     "e2e restart-proof test",
			Source:     "manual",
		},
	}
	require.NoError(t, h.K8s.Create(context.Background(), hold), "create manual SessionHold")
}

// touchSession patches a harmless annotation onto the AgentSession, forcing
// a watch-triggered reconcile without sending a new inbound message (which a
// Held session refuses at the channel layer before it ever reaches the
// runner — see pkg/agent/session/lifecycle/continuation.go's PhaseHeld arm,
// not a route into reconcile at all).
func touchSession(t *testing.T, h *e2e.Harness, ns, name string) {
	t.Helper()
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &sess))
	if sess.Annotations == nil {
		sess.Annotations = map[string]string{}
	}
	sess.Annotations["e2e-restart-test/touch"] = time.Now().Format(time.RFC3339Nano)
	require.NoError(t, h.K8s.Update(context.Background(), &sess))
	// Give the watch-triggered reconcile a moment to actually run before the
	// caller inspects anything.
	time.Sleep(200 * time.Millisecond)
}

// getSessionPhase re-fetches the AgentSession's CURRENT status.phase — a
// fresh Get, never a cached value from before whatever the test just did.
func getSessionPhase(t *testing.T, h *e2e.Harness, ns, name string) string {
	t.Helper()
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &sess))
	return sess.Status.Phase
}

// TestSessionHold_SurvivesOperatorRestart proves the design's central claim
// directly: a held session's phase lives in the SessionHold + AgentSession
// CRs, not in the operator process's own memory. RestartOperator discards
// the manager and Reconciler that parked this session and boots a genuinely
// new pair — zero shared Go-level state with the old one beyond the durable
// dependencies (the same API server, the same memory store) — and the
// session must read Held from both a direct Get and the new manager's own
// independent reconcile.
func TestSessionHold_SurvivesOperatorRestart(t *testing.T) {
	h, ns, name := sessionHoldFixture(t)

	createManualHold(t, h, ns, name, "restart-proof-hold")
	h.WaitForSessionPhase(ns, name, spiceboxv1alpha1.AgentSessionPhaseHeld, 30*time.Second)

	h.RestartOperator(t)

	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseHeld, getSessionPhase(t, h, ns, name),
		"phase must already read Held immediately after restart, with no reconcile of this test's own needed to reassert it")

	// The NEW manager's own reconcile loop — driven by its initial sync
	// against the existing SessionHold, not by anything this test forces —
	// must independently agree.
	h.WaitForSessionPhase(ns, name, spiceboxv1alpha1.AgentSessionPhaseHeld, 30*time.Second)
}

// TestSessionHold_SurvivesRunnerPodDeleteAndRecreate proves the second half
// of containment survives runner churn, not just operator churn.
//
// InProcessRunnerFactory has no real Pod object for an ordinary (non
// identity-choice) session — see inprocess_runner_factory.go's
// identityGatePending gating — so there is no literal Pod this harness can
// delete and recreate. What IS testable, and is the property that actually
// matters, is the operator's own refusal to re-provision: reconcileHold
// returns early, before ANY provisioning logic, for as long as the
// SessionHold is unreleased — regardless of how many times, or why, the
// AgentSession gets reconciled again. Forcing repeated reconciles (standing
// in for "kubelet brought a new pod up and the operator noticed") and
// showing the runner never restarts is the direct proof of that.
func TestSessionHold_SurvivesRunnerPodDeleteAndRecreate(t *testing.T) {
	h, ns, name := sessionHoldFixture(t)

	createManualHold(t, h, ns, name, "restart-proof-hold")
	h.WaitForSessionPhase(ns, name, spiceboxv1alpha1.AgentSessionPhaseHeld, 30*time.Second)

	require.False(t, h.RunnerFactory().IsRunning(ns, name),
		"the hold's own reap (reconcileHold -> reapSessionPods -> RunnerFactory.Stop) must already have stopped the runner — this harness's stand-in for the pod being gone")

	// "Recreated": force two more independent reconcile passes and confirm
	// NEITHER re-provisions a runner. One pass proves nothing by itself —
	// exactly the "single blocked call" trap the bundle rules warn about,
	// applied here to reconciles instead of tool calls.
	touchSession(t, h, ns, name)
	assert.False(t, h.RunnerFactory().IsRunning(ns, name), "a held session must not get a runner re-provisioned on a later reconcile")
	touchSession(t, h, ns, name)
	assert.False(t, h.RunnerFactory().IsRunning(ns, name), "...nor a third time — this must not be a one-shot guard that a second pass slips past")

	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseHeld, getSessionPhase(t, h, ns, name))
}

// TestSessionHold_ReachesHeldWithNoAttemptedBusPublish proves the weaker of
// two related claims: Held is reached when this harness's AgentSession
// reconciler has no RevokePublisher wired at all (test/e2e/harness.go never
// assigns one), so revocation.Publisher.Emit on a nil receiver is a
// guaranteed no-op (see pkg/authz/revocation/publisher.go) and no fast-path
// publish is even attempted. Rather than trust that by reading the wiring,
// this test SUBSCRIBES to the exact subject the fast path would use and
// asserts zero messages arrived, then shows containment happened anyway,
// within one reconcile's worth of wall-clock time.
//
// It does NOT prove the stronger claim that justifies rejecting a bus-only
// design (see the spec's "Rejected alternatives"): that Held is still reached
// when a publish IS attempted and FAILS. That claim needs a wired-but-failing
// publisher, which this e2e harness does not configure; it is covered by
// TestReconcileHold_fastPathPublishFails_stillReachesHeld
// (pkg/controllers/agentsession/hold_test.go), a unit test against a
// fakeBus{fail: true}.
func TestSessionHold_ReachesHeldWithNoAttemptedBusPublish(t *testing.T) {
	h, ns, name := sessionHoldFixture(t)

	var mu sync.Mutex
	seen := 0
	sub, err := h.NATS().Subscribe(subjects.Revocation, func(*nats.Msg) {
		mu.Lock()
		seen++
		mu.Unlock()
	})
	require.NoError(t, err, "subscribe to the revocation subject")
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, h.NATS().Flush())

	createManualHold(t, h, ns, name, "restart-proof-hold")

	// "Within one reconcile": a short deadline, not a retry budget. If
	// containment depended on the bus (or on more than one reconcile pass),
	// this would time out rather than pass.
	h.WaitForSessionPhase(ns, name, spiceboxv1alpha1.AgentSessionPhaseHeld, 5*time.Second)

	require.NoError(t, h.NATS().Flush())
	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, seen, "the bus envelope was never published, and containment must not have needed it to be")
}
