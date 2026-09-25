//go:build e2e

package p2_end_to_end_git_claude_git_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestP2_EndToEnd_GitClaudeGit is the headline Plan-1 + Plan-2 composing
// scenario. The LLM emits three tool_use turns driven from a single
// channel-side user message:
//
//  1. tool_use(git_git, args=[clone, …])    — non-interactive sandbox call
//  2. tool_use(codey_codey, args=[run])     — interactive bridge round-trip
//  3. tool_use(git_git, args=[push, …])     — non-interactive sandbox call
//
// followed by respond_to_user("pushed: abc1234") + EndTurn. The git bundle
// is built-in (no SpiceboxToolkit CR); codey is the same CR-backed
// fake-codey-toolkit Scenario 3 introduced.
//
// The test composes assertions from Scenarios 1, 2, and 3:
//
//   - Scenario 1: One RWX PVC <sess>-workspace owned by the AgentSession.
//   - Scenario 2: At least 2 fake exec calls against the git pod's "sandbox"
//     container; both carry GIT_CONFIG_GLOBAL=/dev/null (and the other two
//     bundleDefaultEnv("git") hardening keys) — surfaced by the AgentSession
//     reconciler's DefaultEnv-stamping path through the ToolCall controller's
//     mergeEnv into exec.Request.Env.
//   - Scenario 3: The scripted-codey driver runs to completion (initial
//     "Reading repo." delta + post-stdin "Patched." + terminal delta).
//
// This is the original goal made concrete: a user message in a channel
// drives a multi-bundle agent through a real interactive sub-session, with
// pre-/post-interactive non-interactive tool calls on a sibling bundle
// sharing the workspace PVC. Every layer (channelsd, runner, sandbox tool,
// ToolCall controller, fake exec, interactive bridge, ScriptedLLM, fake
// channel) participates.
func TestP2_EndToEnd_GitClaudeGit(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		WithToolCallController: true,
		WorkspaceStorageClass:  "rwx-test",
		ExtraManifests:         []string{string(manifests)},
		DefaultTimeout:         20 * time.Second,
	})

	// Build the scripted codey driver BEFORE installing it on fakeExec
	// (the install happens inside stampBundleSessionsReady when the codey
	// SpiceboxSession appears, BEFORE its Ready=True flip — same race
	// rationale as Scenario 3).
	//
	// Simpler than Scenario 3: one human-paired interaction. The driver
	// emits an initial "Reading repo." (the bridge's first delta), waits
	// for stdin containing "apply", then emits "Patched." + exits 0.
	codey := e2e.NewScriptedInteractiveTool(t)
	codey.EmitInitial(func(em *e2e.Emitter) {
		em.Stdout([]byte("Reading repo.\n"))
	})
	codey.OnStdin(e2e.StdinContains("apply"), func(em *e2e.Emitter) {
		em.Stdout([]byte("Patched.\n"))
		em.Exit(0)
	})

	// All background goroutines join via the done-channel cleanup
	// pattern (Task 7's lesson). The harness cancels its own bits, but
	// helper goroutines spawned here must be joined by us.
	stamperDone := make(chan struct{})
	programDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		<-stamperDone
		<-programDone
	})

	// 1. Stamp both SpiceboxToolspecs Valid=True so the AgentClass
	// binding-coverage check accepts ts-git and ts-codey.
	stampToolspecsValid(t, ctx, h.K8s, "ts-git", "ts-codey")

	// 2. Background loop that stamps every bundle SpiceboxSession Ready=True
	// + PodName + ResolvedClass + EffectiveToolspecs the moment it
	// appears, AND installs the scripted-codey driver onto fakeExec
	// for the codey bundle's pod key BEFORE flipping Ready=True (so the
	// streaming bridge never sees a key with no programmed driver).
	go func() {
		defer close(stamperDone)
		stampBundleSessionsReady(ctx, t, h, codey)
	}()

	// 3. Program a canned response for the git bundle's pod key the
	// moment its SpiceboxSession appears. Program is persistent (a map
	// lookup, not consumed), so one Program call serves both the clone
	// AND the push calls — they hit the same "<sess>-git-pod:sandbox"
	// key on the same fake.Binder. The actual git subcommand doesn't
	// matter to the fake (key is namespace/pod:container).
	go func() {
		defer close(programDone)
		programGitPodResponse(ctx, t, h)
	}()

	// 4. LLM script. Same operation_id round-trip pattern Scenario 2
	// introduced: new_operation mints an id, every subsequent sandbox
	// tool_use must echo it back via operation_id.
	//
	// Rule registration order matters — ScriptedLLM walks rules in
	// order and consumes the first match. The two OnToolResult("git_git",
	// …) rules sit in clone-then-push order; the codey result between
	// them gates the second git_git rule from firing on the clone
	// result.
	var capturedOpID string

	h.LLM.OnUserMessage("fix the failing test in widgets and push").Reply(
		e2e.ToolUse("new_operation", map[string]any{
			"description": "fix failing widgets test and push the fix",
		}))

	// operation_id capture (side-effecting predicate).
	h.LLM.OnToolResult("new_operation", e2e.ResultMatches(func(got any) bool {
		if m, ok := got.(map[string]any); ok {
			if id, ok := m["operation_id"].(string); ok {
				capturedOpID = id
			}
		}
		return true
	})).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("git_git", map[string]any{
			"operation_id": capturedOpID,
			"_reason":      "clone the widgets repo to inspect the failing test",
			"args":         []string{"clone", "https://example.com/widgets.git"},
		})}
	})

	// First git_git result (clone) → interactive codey turn.
	h.LLM.OnToolResult("git_git", e2e.AnyResult()).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("codey_codey", map[string]any{
			"operation_id": capturedOpID,
			"_reason":      "drive an interactive codey session to patch the failing test",
			"args":         []string{"run"},
		})}
	})

	// codey result → second git_git (push).
	h.LLM.OnToolResult("codey_codey", e2e.AnyResult()).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("git_git", map[string]any{
			"operation_id": capturedOpID,
			"_reason":      "push the fix",
			"args":         []string{"push", "origin", "main"},
		})}
	})

	// Second git_git result (push) → final respond_to_user.
	h.LLM.OnToolResult("git_git", e2e.AnyResult()).Reply(
		e2e.RespondToUser("pushed: abc1234"))

	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.WaitForAgentClassValid("ac-e2e", 30*time.Second)
	h.SendUserMessage("fix the failing test in widgets and push")

	// 5. Drive the interactive bridge round-trip in the middle of the
	// flow. authz.toolCalls.mode=disabled bypasses the Check on codey's
	// "external" stateImpact AND on the git tools — no approval prompts
	// to consume.
	initial := h.ExpectToolSessionDelta(e2e.ToolSessionContains("Reading repo."))
	ref := initial.ToolCallRef
	require.NotEmpty(t, ref, "initial delta must carry a ToolCallRef")

	h.SendChannelInputToToolSession(ref, "apply the fix")
	h.ExpectToolSessionDelta(e2e.And(
		e2e.ToolCallRef(ref),
		e2e.ToolSessionContains("Patched."),
	))
	h.ExpectToolSessionDelta(e2e.And(
		e2e.ToolCallRef(ref),
		e2e.Terminal("completed"),
	))

	h.ExpectAgentReply(e2e.Contains("pushed: abc1234"))

	// 6. Composing assertions — the headline payoff.

	// (a) Scenario 1's assertion: one RWX PVC owned by the AgentSession.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(ctx, &sessions, client.InNamespace("default")),
		"list AgentSessions")
	require.NotEmpty(t, sessions.Items, "at least one AgentSession created by the channelsd pipeline")
	sess := sessions.Items[0]

	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{
		Namespace: "default", Name: sess.Name + "-workspace",
	}, &pvc), "workspace PVC should exist")
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, pvc.Spec.AccessModes,
		"PVC AccessModes")
	require.Len(t, pvc.OwnerReferences, 1, "PVC must be owned by AgentSession")
	assert.Equal(t, sess.Name, pvc.OwnerReferences[0].Name, "PVC owner name")
	assert.Equal(t, "AgentSession", pvc.OwnerReferences[0].Kind, "PVC owner kind")

	// (b) Scenario 2's assertion: ≥2 fake exec calls against the git
	// pod's "sandbox" container, each carrying the hardening DefaultEnv
	// keys. The harness's fake exec doesn't distinguish between
	// subcommands (key is namespace/pod:container), so we filter by pod
	// name to pick the git calls out of the broader Calls slice — the
	// codey call also lands on "sandbox" but on a different pod.
	gitPodName := sess.Name + "-git-pod"
	calls := h.FakeExec().Calls()
	require.GreaterOrEqual(t, len(calls), 2,
		"expected at least 2 fake exec calls (git clone + git push); got %d", len(calls))

	gitCalls := filterCallsByPod(calls, gitPodName)
	require.GreaterOrEqual(t, len(gitCalls), 2,
		"expected at least 2 fake exec calls against the git pod %q; got %d (calls=%+v)",
		gitPodName, len(gitCalls), calls)
	for i, c := range gitCalls {
		assert.Equal(t, "/dev/null", c.Request.Env["GIT_CONFIG_GLOBAL"],
			"gitCalls[%d]: GIT_CONFIG_GLOBAL must reach exec via SpiceboxSession.DefaultEnv", i)
		assert.Equal(t, "1", c.Request.Env["GIT_CONFIG_NOSYSTEM"],
			"gitCalls[%d]: GIT_CONFIG_NOSYSTEM must reach exec via SpiceboxSession.DefaultEnv", i)
		assert.Equal(t, "0", c.Request.Env["GIT_TERMINAL_PROMPT"],
			"gitCalls[%d]: GIT_TERMINAL_PROMPT must reach exec via SpiceboxSession.DefaultEnv", i)
	}

	// (c) Scenario 3's assertion: the scripted-codey rules all fired.
	codey.AssertAllRulesConsumed()

	// (d) The LLM script ran to completion (every rule matched).
	h.AssertAllRulesConsumed()
}

// filterCallsByPod returns the subset of fake.Calls whose Pod matches pod.
// Used to separate the git pod's exec calls from the codey pod's streaming
// exec call (both land on Container=sandbox).
func filterCallsByPod(calls []fake.Call, pod string) []fake.Call {
	out := make([]fake.Call, 0, len(calls))
	for _, c := range calls {
		if c.Pod == pod {
			out = append(out, c)
		}
	}
	return out
}

// stampToolspecsValid sets Status.Conditions[Valid]=True on each named
// SpiceboxToolspec. Lifted from Scenarios 1/2/3 — promotion into a shared
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
// controller need: PodName, ResolvedClass, EffectiveToolspecs, then
// Ready=True. For the codey bundle ONLY, also installs the scripted
// driver onto fakeExec for the bundle's pod key BEFORE flipping
// Ready=True (otherwise the streaming bridge dials a key with no
// programmed driver and the bridge sees an immediate EOF).
//
// Mirrors Scenario 3's helper of the same name; extended for the two-
// bundle case (the loop processes both git and codey but only installs
// the scripted driver on codey).
func stampBundleSessionsReady(
	ctx context.Context,
	t *testing.T,
	h *e2e.Harness,
	codey *e2e.ScriptedInteractiveTool,
) {
	c := h.K8s
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	installedFor := map[string]bool{} // codey session name → already installed?
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
			// bundle's pod key BEFORE flipping Ready=True. The git pod
			// programming is handled in a separate goroutine
			// (programGitPodResponse) since it uses Program rather than
			// ProgramStreamFunc.
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

// programGitPodResponse polls for the git bundle's SpiceboxSession, then
// programs fakeExec with a canned response for the pod's "sandbox"
// container. Program is persistent (the fake stores a map, not a queue) —
// one Program call serves BOTH the clone and the push exec calls, which
// share the same pod-key. Returns once the response is programmed, or ctx
// is canceled. Lifted from Scenario 2.
func programGitPodResponse(ctx context.Context, t *testing.T, h *e2e.Harness) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var list spiceboxv1alpha1.SpiceboxSessionList
		if err := h.K8s.List(ctx, &list, client.InNamespace("default")); err != nil {
			if ctx.Err() != nil {
				return
			}
			t.Logf("programGitPodResponse: list: %v", err)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			if s.Labels["agentprimitives.authzed.com/agentbundle"] != "git" {
				continue
			}
			if s.Status.PodName == "" {
				continue
			}
			key := s.Namespace + "/" + s.Status.PodName + ":sandbox"
			h.FakeExec().Program(key, fake.Response{
				Stdout:   []byte("ok\n"),
				ExitCode: 0,
			})
			return
		}
	}
}
