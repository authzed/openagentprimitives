//go:build e2e

package p2_interactive_happy_path_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestP2_InteractiveHappyPath exercises the full Plan-2 interactive bridge
// round-trip end-to-end. The LLM issues a single interactive sandbox tool
// (`codey_codey`) — synthesized from the cluster-stored fake-codey-toolkit
// SpiceboxToolkit CR plus a single-allowed-interactive-subcommand toolspec
// (ts-codey, AllowSubcommands=[run]). The synthesizer wires Interactive=true
// on the resulting Tool, which routes execution through SandboxTool's
// executeInteractive path:
//
//	tool_use(codey_codey, …)
//	  → SandboxTool.executeInteractive creates a Mode=interactive ToolCall
//	  → ToolCall controller's streaming reconcile path issues StreamExec
//	  → fakeExec's ScriptedInteractiveTool driver runs (emits "Ready.")
//	  → controller registers an ActiveStream on the in-process gateway
//	  → SandboxTool's bridge dials the gateway (via the harness's bufconn
//	    dial-opt threaded onto InteractiveHooks.BridgeDialOpts) and pumps
//	    stdout/stdin between the channel and the fake exec
//
// On the channel side, ExpectToolSessionDelta blocks for the bridge's
// PublishOut(KindToolSessionDelta) envelopes (initial "Ready.",
// post-stdin "Applying fix.", post-stdin "Done.", terminal). Each
// SendChannelInputToToolSession publishes an inbound KindToolSessionInput
// envelope; the runner's per-session tool_session subscriber routes the
// bytes through bridge.Feed, which writes them onto the gateway stream's
// Stdin queue, which the fake exec's stdin pipe surfaces to the scripted
// driver's bufio.Scanner.
//
// Use the authz.toolCalls.mode=disabled rationale from Scenario 2 — no SpiceDB
// wired in the harness, so the default enforcing mode would deny on the
// "external" stateImpact without a guardian grant. Auto-approve is the
// effect.
//
// The test asserts:
//
//   - Initial delta arrives with "Ready." (captures the ToolCallRef).
//   - After "fix the test" → "Applying fix." delta on the same ToolCallRef.
//   - After "commit it"   → "Done."        delta on the same ToolCallRef.
//   - Terminal delta with ExitReason="completed" on the same ToolCallRef.
//   - Agent reply contains "interactive session finished cleanly".
//   - Both the scripted-codey rules and the LLM script ran to completion.
func TestP2_InteractiveHappyPath(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		WithToolCallController: true,
		// No WorkspaceStorageClass: the codey bundle doesn't need a
		// shared PVC. WorkspaceStorageClass gates per-AgentSession PVC
		// stamping that Scenario 1 exercises; this scenario is the
		// per-bundle stream path.
		ExtraManifests: []string{string(manifests)},
		DefaultTimeout: 20 * time.Second,
	})

	// Build the scripted interactive tool BEFORE installing it on
	// fakeExec. Registering rules later (after the bridge dials) would
	// race against the driver's "is initial set yet?" read; rules
	// must exist before the controller's StreamExec invokes the driver.
	codey := e2e.NewScriptedInteractiveTool(t)
	codey.EmitInitial(func(em *e2e.Emitter) {
		em.Stdout([]byte("Ready.\n"))
	})
	codey.OnStdin(e2e.StdinContains("fix the test"), func(em *e2e.Emitter) {
		em.Stdout([]byte("Applying fix.\n"))
	})
	codey.OnStdin(e2e.StdinContains("commit it"), func(em *e2e.Emitter) {
		em.Stdout([]byte("Done.\n"))
		em.Exit(0)
	})

	// All background goroutines join via the done-channel cleanup
	// pattern (Task 7's lesson; the harness cancels its own bits, but
	// helper goroutines we spawn here must be joined by us).
	stamperDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		<-stamperDone
	})

	// 1. Stamp the SpiceboxToolspec Valid=True so AgentClass binding-
	// coverage accepts ts-codey. (The harness doesn't wire the
	// SpiceboxToolspec controller — same as Scenario 1/2.)
	stampToolspecsValid(t, ctx, h.K8s, "ts-codey")

	// 2. Background loop stamps each bundle SpiceboxSession Ready=True +
	// PodName + ResolvedClass + EffectiveToolspecs the moment it
	// appears. CRITICAL: we install the scripted tool's driver onto
	// fakeExec BEFORE flipping Ready=True so the controller's
	// reconcileStreaming path (triggered by the Mode=interactive
	// ToolCall the runner creates downstream) finds a programmed
	// stream-func and the bridge sees "Ready." rather than a default
	// empty-stream EOF.
	go func() {
		defer close(stamperDone)
		stampBundleSessionsReady(ctx, t, h, codey)
	}()

	// 3. LLM script: same operation_id round-trip pattern as Scenario 2.
	//   "please fix the failing test" → tool_use(new_operation, …)
	//   tool_result(new_operation, _)  → tool_use(codey_codey, args=[run],
	//                                              operation_id=<captured>)
	//   tool_result(codey_codey, _)    → respond_to_user("interactive session finished cleanly")
	//   tool_result(respond_to_user,_) → EndTurn
	//
	// args=["run"] because the SpiceboxClass tool's command is
	// ["/usr/bin/codey"]; the controller appends tc.Spec.Args, yielding
	// ["/usr/bin/codey", "run"]. The fake exec keys by namespace/pod:
	// container only, so the command vector doesn't gate routing — but
	// it matches what production would emit.
	var capturedOpID string
	h.LLM.OnUserMessage("please fix the failing test").Reply(e2e.ToolUse("new_operation", map[string]any{
		"description": "run an interactive codey session",
	}))
	h.LLM.OnToolResult("new_operation", e2e.ResultMatches(func(got any) bool {
		if m, ok := got.(map[string]any); ok {
			if id, ok := m["operation_id"].(string); ok {
				capturedOpID = id
			}
		}
		return true
	})).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("codey_codey", map[string]any{
			"operation_id": capturedOpID,
			"_reason":      "drive an interactive codey session",
			"args":         []string{"run"},
		})}
	})
	h.LLM.OnToolResult("codey_codey", e2e.AnyResult()).Reply(
		e2e.RespondToUser("interactive session finished cleanly"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.WaitForAgentClassValid("ac-codey", 30*time.Second)
	h.SendUserMessage("please fix the failing test")

	// 4. Drive the bridge round-trip.
	//
	// The toolkit's run subcommand declares stateImpact=external; the
	// runner-side authz path would normally Check + (on deny) publish a
	// tool_approval_request. With authz.toolCalls.mode=disabled the Check is
	// skipped entirely — no ExpectApprovalPrompt needed. (The runner
	// re-confirms the disabled mode at session start via the
	// AgentClassConditionToolAuthDisabled status condition; this scenario
	// inherits that behavior from Scenario 2.)

	initial := h.ExpectToolSessionDelta(e2e.ToolSessionContains("Ready."))
	ref := initial.ToolCallRef
	require.NotEmpty(t, ref, "initial delta must carry a ToolCallRef")

	h.SendChannelInputToToolSession(ref, "fix the test")
	h.ExpectToolSessionDelta(e2e.And(
		e2e.ToolCallRef(ref),
		e2e.ToolSessionContains("Applying fix."),
	))

	h.SendChannelInputToToolSession(ref, "commit it")
	h.ExpectToolSessionDelta(e2e.And(
		e2e.ToolCallRef(ref),
		e2e.ToolSessionContains("Done."),
	))

	h.ExpectToolSessionDelta(e2e.And(
		e2e.ToolCallRef(ref),
		e2e.Terminal("completed"),
	))

	h.ExpectAgentReply(e2e.Contains("interactive session finished cleanly"))

	codey.AssertAllRulesConsumed()
	h.AssertAllRulesConsumed()
}

// stampToolspecsValid sets Status.Conditions[Valid]=True on each named
// SpiceboxToolspec. Lifted from Scenario 2 — promotion into a shared
// helper is a deliberate follow-up once the scenario shape settles.
func stampToolspecsValid(t *testing.T, ctx context.Context, c client.Client, names ...string) {
	t.Helper()
	for _, name := range names {
		var ts spiceboxv1alpha1.SpiceboxToolspec
		require.NoError(t, c.Get(ctx, client.ObjectKey{Name: name}, &ts),
			"get toolspec %q", name)
		ts.Status.Conditions = []metav1.Condition{{
			Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
			Status:             metav1.ConditionTrue,
			Reason:             "Resolved",
			LastTransitionTime: metav1.Now(),
		}}
		require.NoError(t, c.Status().Update(ctx, &ts), "stamp toolspec %q Valid=True", name)
	}
}

// stampBundleSessionsReady polls for SpiceboxSessions in the default
// namespace and stamps them with the fields the runner + ToolCall
// controller need.
//
// Critical ordering: the scripted tool is installed onto fakeExec for
// the codey bundle's pod key BEFORE Ready=True is stamped. If we flipped
// Ready first, the AgentSession would mark BundlesReady, the runner
// would dispatch the codey_codey tool_use, the sandbox tool would create
// a Mode=interactive ToolCall, the controller's reconcileStreaming would
// call StreamExec on a key with NO programmed driver, and the resulting
// empty-stream EOF would fail the bridge before the test could send
// stdin.
//
// Mirrors Scenario 2's helper of the same name, extended with the
// codey-driver-install step.
func stampBundleSessionsReady(
	ctx context.Context,
	t *testing.T,
	h *e2e.Harness,
	codey *e2e.ScriptedInteractiveTool,
) {
	c := h.K8s
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	installedFor := map[string]bool{} // session name → already installed?
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var list spiceboxv1alpha1.SpiceboxSessionList
		if err := c.List(ctx, &list, client.InNamespace("default")); err != nil {
			if ctx.Err() != nil {
				return
			}
			t.Logf("stampBundleSessionsReady: list: %v", err)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			dirty := false

			if s.Status.PodName == "" {
				s.Status.PodName = s.Name + "-pod"
				s.Status.Sandbox = &spiceboxv1alpha1.SandboxHandle{
					Kind: pod.KindName,
					Ref:  s.Namespace + "/" + s.Status.PodName,
				}
				dirty = true
			}
			if s.Status.ResolvedClass == nil {
				var cls spiceboxv1alpha1.SpiceboxClass
				if err := c.Get(ctx, client.ObjectKey{Name: s.Spec.Class}, &cls); err != nil {
					if ctx.Err() != nil {
						return
					}
					t.Logf("stampBundleSessionsReady: get class %q for %s/%s: %v",
						s.Spec.Class, s.Namespace, s.Name, err)
					continue
				}
				rc := cls.Spec.DeepCopy()
				s.Status.ResolvedClass = rc
				dirty = true
			}
			if len(s.Status.EffectiveToolspecs) == 0 {
				if len(s.Spec.Toolspecs) > 0 {
					eff := make([]string, 0, len(s.Spec.Toolspecs))
					for _, tr := range s.Spec.Toolspecs {
						eff = append(eff, tr.Name)
					}
					s.Status.EffectiveToolspecs = eff
				} else if s.Status.ResolvedClass != nil {
					eff := make([]string, 0, len(s.Status.ResolvedClass.Toolspecs))
					for _, tr := range s.Status.ResolvedClass.Toolspecs {
						eff = append(eff, tr.Name)
					}
					s.Status.EffectiveToolspecs = eff
				}
				if len(s.Status.EffectiveToolspecs) > 0 {
					dirty = true
				}
			}

			// Install the scripted driver onto fakeExec for the codey
			// bundle's pod key BEFORE flipping Ready=True. See helper
			// docstring for the race rationale.
			if s.Labels["agentprimitives.authzed.com/agentbundle"] == "codey" &&
				s.Status.PodName != "" && !installedFor[s.Name] {
				key := s.Namespace + "/" + s.Status.PodName + ":sandbox"
				codey.Install(h.FakeExec(), key)
				installedFor[s.Name] = true
			}

			if !meta.IsStatusConditionTrue(s.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionReady) {
				s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{
					Type:               spiceboxv1alpha1.SpiceboxSessionConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             spiceboxv1alpha1.ReasonPodReady,
					Message:            "stamped Ready by e2e harness helper",
					LastTransitionTime: metav1.Now(),
				})
				dirty = true
			}

			if !dirty {
				continue
			}
			if err := c.Status().Update(ctx, s); err != nil {
				if ctx.Err() != nil {
					return
				}
				t.Logf("stampBundleSessionsReady: update %s/%s: %v",
					s.Namespace, s.Name, err)
			}
		}
	}
}
