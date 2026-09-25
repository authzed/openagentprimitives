package runner

// identity_boot_test.go covers Task 6's runner-side identity wiring that the
// review flagged: the SessionStart Halt → terminal-reason mapping
// (identityHaltReason + its use in loop.Run) and BundleRuntimeIdentity honoring
// the RESOLVED effective mode rather than class.Spec.IdentityMode.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// TestIdentityHaltReason is the focused unit test for the SessionStart Halt →
// terminal-reason mapping. An identity-gate halt must NEVER terminalize as
// ScopeReviewFailed; cold-start scope halts keep it.
func TestIdentityHaltReason(t *testing.T) {
	cases := []struct {
		name       string
		gateReason string
		want       string
	}{
		{"cancel → IdentityChoiceCancelled", "identity_cancelled", spiceboxv1alpha1.ReasonIdentityChoiceCancelled},
		{"timeout → IdentityChoiceTimeout", "identity_choice_timeout", spiceboxv1alpha1.ReasonIdentityChoiceTimeout},
		{"non-interactive fail-closed → IdentityChoiceFailed", "identity_choice_unavailable", spiceboxv1alpha1.ReasonIdentityChoiceFailed},
		{"build failure fail-closed → IdentityChoiceFailed", "identity_choice_build_failed", spiceboxv1alpha1.ReasonIdentityChoiceFailed},
		{"await failure fail-closed → IdentityChoiceFailed", "identity_choice_await_failed", spiceboxv1alpha1.ReasonIdentityChoiceFailed},
		{"unknown action fail-closed → IdentityChoiceFailed", "identity_choice_unknown_action", spiceboxv1alpha1.ReasonIdentityChoiceFailed},
		{"cold-start halt keeps ScopeReviewFailed", "cold-start: publish scope-review request: boom", spiceboxv1alpha1.ReasonAgentSessionScopeReviewFailed},
		{"empty reason (defensive) keeps ScopeReviewFailed", "", spiceboxv1alpha1.ReasonAgentSessionScopeReviewFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, identityHaltReason(tc.gateReason))
		})
	}
}

// buildIdentityBootLoop wires a *Loop that reaches the SessionStart
// IdentityChoiceGate on Run: IdentityGatePending set, scope disabled (so the gate
// runs standalone — no cold-start), a real approval orchestrator, and a capturing
// publish that (when deliver != nil) answers with a canned Action from a
// goroutine once it has the minted reqID. The empty llmfake provider makes any
// agent-loop LLM call a test failure — so cancel/timeout/handoff must stop the
// runner before the agent runs.
func buildIdentityBootLoop(t *testing.T, deliver string, hasDeliver bool) *Loop {
	t.Helper()
	key := memory.NamespacedName{Namespace: "default", Name: "idboot1"}
	mem := buildLifecycleMemory()
	orch := approval.New()

	l := &Loop{
		SessionKey:            key,
		Memory:                LocalMemoryAdapter(mem, key),
		Mem:                   mem,
		LifecycleMemory:       mem,
		Provider:              llmfake.New(nil), // empty script: any Send is a test failure
		Status:                LocalStatusPatcher(),
		Budget:                NewBudget(spiceboxv1alpha1.BudgetConfig{}, nil, time.Now()),
		Approval:              orch,
		IdentityGatePending:   true,
		IdentityChoiceTimeout: 2 * time.Second,
		AgentName:             "helper",
		ChannelKind:           "slack",
		LastInboundExternalID: "U-requester",
		UserPrompt:            "do the thing",
		// scope disabled → coldStartEligibility() is false → only the gate runs.
		AgentClass: &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{}},
	}
	l.IdentityChoicePublish = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
		if env.Kind != channelevents.KindInteractionRequest {
			// The synthetic timeout-applied publish (KindInteractionApplied) isn't
			// relevant here — this fixture drives the decision straight into the
			// orchestrator (simulating channelsd's decision bridge) rather than
			// round-tripping through a real Applied envelope.
			return nil
		}
		if !hasDeliver {
			return nil // no delivery → Await times out
		}
		var pl channelevents.InteractionRequestPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return err
		}
		go orch.DeliverDecision(pl.RequestRef, approval.Decision{Action: deliver, ApproverID: "U-approver"})
		return nil
	}
	return l
}

// TestRunIdentityGate_TerminalReason drives loop.Run through the SessionStart
// gate end-to-end and asserts the terminal outcome per choice: cancel/timeout
// terminalize with their dedicated reasons (never ScopeReviewFailed), and the
// passthrough handoff exits NON-terminally (no Failed write, IdentityHandoffExited
// set). In every case the agent LLM loop must NEVER run.
func TestRunIdentityGate_TerminalReason(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	t.Run("cancel: Failed/IdentityChoiceCancelled, agent never runs", func(t *testing.T) {
		l := buildIdentityBootLoop(t, "cancel", true)
		require.NoError(t, l.Run(ctx), "Run returns nil on a clean terminal write")

		fail := l.Status.LocalFailure()
		require.NotNil(t, fail, "cancel must mark the session failed")
		assert.Equal(t, spiceboxv1alpha1.ReasonIdentityChoiceCancelled, fail.Reason)
		assert.NotEqual(t, spiceboxv1alpha1.ReasonAgentSessionScopeReviewFailed, fail.Reason)
		assert.False(t, l.IdentityHandoffExited, "cancel is not a handoff")
		assert.Empty(t, l.Provider.(*llmfake.Provider).Requests(), "the agent LLM loop must not run")
	})

	t.Run("timeout: Failed/IdentityChoiceTimeout, agent never runs", func(t *testing.T) {
		l := buildIdentityBootLoop(t, "", false)
		l.IdentityChoiceTimeout = 60 * time.Millisecond // resolve the wait fast
		require.NoError(t, l.Run(ctx))

		fail := l.Status.LocalFailure()
		require.NotNil(t, fail, "timeout must mark the session failed")
		assert.Equal(t, spiceboxv1alpha1.ReasonIdentityChoiceTimeout, fail.Reason)
		assert.NotEqual(t, spiceboxv1alpha1.ReasonAgentSessionScopeReviewFailed, fail.Reason)
		assert.Empty(t, l.Provider.(*llmfake.Provider).Requests(), "the agent LLM loop must not run")
	})

	t.Run("passthrough handoff: non-terminal exit, no Failed write", func(t *testing.T) {
		l := buildIdentityBootLoop(t, "userPassthrough", true)
		require.NoError(t, l.Run(ctx), "handoff exits Run cleanly (nil)")

		assert.True(t, l.IdentityHandoffExited, "handoff must set IdentityHandoffExited")
		assert.Nil(t, l.Status.LocalFailure(), "handoff must NOT write a terminal Failed status")
		assert.Empty(t, l.Provider.(*llmfake.Provider).Requests(), "the agent LLM loop must not run")
	})
}

// TestBundleRuntimeIdentity_HonorsEffectiveMode proves the Fix-B invariant: with
// effectiveMode=userPassthrough the helper returns the passed session
// (passthrough) identity even when class.Spec.IdentityMode is ask|dynamic — it
// must NOT fall through to the class AgentIdentity. effectiveMode=agent still
// resolves the class AgentIdentity.
func TestBundleRuntimeIdentity_HonorsEffectiveMode(t *testing.T) {
	ns := "sess-ns"
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	agentAI := &spiceboxv1alpha1.AgentIdentity{}
	agentAI.Name = "agent-ai"
	agentAI.Namespace = ns
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agentAI).Build()

	// A distinctive passthrough identity the caller resolved from a SessionUserIdentity.
	sessID := RuntimeIdentity{Label: "SessionUserIdentity sess-passthrough", Namespace: spiceboxv1alpha1.IdentitiesNamespace}

	// The class declares ask + an AgentIdentity (as CEL requires for ask|dynamic).
	class := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode:  spiceboxv1alpha1.IdentityModeAsk,
			AgentIdentity: "agent-ai",
		},
	}
	bundleCfg := spiceboxv1alpha1.ToolBundle{Name: "b1"}

	t.Run("effectiveMode=userPassthrough returns the session identity, not the agent identity", func(t *testing.T) {
		got, err := BundleRuntimeIdentity(context.Background(), c, ns, sessID, class, bundleCfg,
			spiceboxv1alpha1.IdentityModeUserPassthrough)
		require.NoError(t, err)
		assert.Equal(t, sessID.Label, got.Label, "passthrough must reuse the session identity")
		assert.NotEqual(t, "AgentIdentity agent-ai", got.Label, "must NOT resolve the class AgentIdentity")
	})

	t.Run("effectiveMode=agent resolves the class AgentIdentity", func(t *testing.T) {
		got, err := BundleRuntimeIdentity(context.Background(), c, ns, sessID, class, bundleCfg,
			spiceboxv1alpha1.IdentityModeAgent)
		require.NoError(t, err)
		assert.Equal(t, "AgentIdentity agent-ai", got.Label, "agent mode resolves the class AgentIdentity")
	})
}
