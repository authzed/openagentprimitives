//go:build e2e

package p1_defaultenv_reaches_toolcall_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestP1_DefaultEnv_ReachesSandboxToolCall verifies that
// SpiceboxSession.Spec.DefaultEnv (stamped by the AgentSession
// reconciler's bundleDefaultEnv("git")) flows through the ToolCall
// controller's mergeEnv path into the exec.Request.Env — without the
// LLM passing those keys in tool_use args.
//
// The LLM emits a single sandbox tool_use (named "git_git" — the
// synthesized LLM-facing name is "<bundle.name>_<class-tool.name>"; the
// bundle is named "git" and the SpiceboxClass tool is named "git" since
// it maps to the upstream `git` toolkit, so the joined name is "git_git").
// The harness programs a canned response on the fake exec; the asserts
// iterate fakeExec.Calls() and check the git pod's exec saw all three
// hardening env keys.
func TestP1_DefaultEnv_ReachesSandboxToolCall(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		WithToolCallController: true,
		WorkspaceStorageClass:  "rwx-test",
		ExtraManifests:         []string{string(manifests)},
	})

	// done is closed when ALL background goroutines launched by this
	// test have exited. The cleanup function waits on it after cancel
	// so the test doesn't return while helpers are still running
	// (Task 7's lesson: join the done-channel pattern).
	stamperDone := make(chan struct{})
	programDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		<-stamperDone
		<-programDone
	})

	// 1. Stamp the SpiceboxToolspec Valid=True so the AgentClass
	// binding-coverage check accepts ts-git.
	stampToolspecsValid(t, ctx, h.K8s, "ts-git")

	// 2. Background loop that stamps every bundle SpiceboxSession
	// Ready=True the moment it appears, including PodName and
	// ResolvedClass (the ToolCall controller reads both — without them
	// validate() returns SessionNotActive and the call never reaches
	// exec). Idempotent — touching an already-Ready session is a no-op.
	go func() {
		defer close(stamperDone)
		stampBundleSessionsReady(ctx, t, h.K8s)
	}()

	// 3. Program the fake exec for the git pod once the bundle's
	// SpiceboxSession is created. The pod name is "<sess>-git-pod" —
	// we don't know <sess> until the AgentSession controller creates
	// the bundle session, so poll for it.
	go func() {
		defer close(programDone)
		programGitPodResponse(ctx, t, h)
	}()

	// 4. LLM script:
	//     "status" → tool_use(new_operation, …) — mints an operation_id
	//     tool_result(new_operation, _) → tool_use(git_git, …, captured op id, args=[status])
	//     tool_result(git_git, _) → respond_to_user("clean")
	//     tool_result(respond_to_user, _) → EndTurn
	//
	// The sandbox tool_use's operation_id must match what new_operation
	// returned (sess.Operations.Get(...) check in sandbox.Execute), so we
	// resolve it from the prior tool_result via a predicate-with-side-effect
	// that captures the id, and a ReplyFn that injects it at match time.
	//
	// The args literal is ["status"] (the git subcommand). It does NOT
	// include GIT_CONFIG_GLOBAL etc. — that's the whole point: those env
	// keys reach exec via the controller's
	// mergeEnv(agentEnv, mergeEnv(tc.Spec.Env, session.Spec.DefaultEnv)).
	var capturedOpID string
	h.LLM.OnUserMessage("status").Reply(e2e.ToolUse("new_operation", map[string]any{
		"description": "show git status",
	}))
	h.LLM.OnToolResult("new_operation", e2e.ResultMatches(func(got any) bool {
		// Capture the operation_id from new_operation's JSON response so
		// the next tool_use can pass it back. Predicate ALWAYS matches —
		// the capture is a side effect of the predicate evaluation.
		if m, ok := got.(map[string]any); ok {
			if id, ok := m["operation_id"].(string); ok {
				capturedOpID = id
			}
		}
		return true
	})).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("git_git", map[string]any{
			"operation_id": capturedOpID,
			"_reason":      "see what changed",
			"args":         []string{"status"},
		})}
	})
	h.LLM.OnToolResult("git_git", e2e.AnyResult()).Reply(e2e.RespondToUser("clean"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.WaitForAgentClassValid("ac-git", 30*time.Second)
	h.SendUserMessage("status")
	h.ExpectAgentReply(e2e.Contains("clean"))

	// 5. Asserts: locate the exec call against the git pod's "sandbox"
	// container, then check the env merge.
	calls := h.FakeExec().Calls()
	require.NotEmpty(t, calls, "fake exec should have recorded at least one call")

	var gitCall fake.Call
	found := false
	for _, c := range calls {
		if c.Container == "sandbox" {
			gitCall = c
			found = true
			break
		}
	}
	require.True(t, found, "expected an exec call against the git pod (Container=sandbox); got %+v", calls)
	require.NotEmpty(t, gitCall.Pod, "git pod name should be set on the exec request")

	// The three DefaultEnv keys must be present on the exec request
	// even though the LLM did not pass them in tool_use args.
	assert.Equal(t, "/dev/null", gitCall.Request.Env["GIT_CONFIG_GLOBAL"],
		"GIT_CONFIG_GLOBAL must reach the exec via SpiceboxSession.DefaultEnv")
	assert.Equal(t, "1", gitCall.Request.Env["GIT_CONFIG_NOSYSTEM"],
		"GIT_CONFIG_NOSYSTEM must reach the exec via SpiceboxSession.DefaultEnv")
	assert.Equal(t, "0", gitCall.Request.Env["GIT_TERMINAL_PROMPT"],
		"GIT_TERMINAL_PROMPT must reach the exec via SpiceboxSession.DefaultEnv")

	h.AssertAllRulesConsumed()
}

// stampToolspecsValid sets Status.Conditions[Valid]=True on each named
// SpiceboxToolspec so the AgentClass binding-coverage check passes.
// Lifted from Scenario 1 — promotion into a shared helper is a
// deliberate follow-up once the scenario shape settles.
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
// namespace and stamps any without a Ready=True condition. Also fills
// in Status.PodName + Status.ResolvedClass when missing — the ToolCall
// controller reads BOTH in validate() (resolved.session.Status.PodName
// is used to construct the exec.Request and Status.ResolvedClass is
// the tool catalog used to resolve tc.Spec.Tool → command).
//
// Runs until ctx is canceled. Idempotent — already-stamped fields are
// not rewritten on subsequent ticks.
//
// Lifted (and extended) from Scenario 1 — Scenario 1 didn't need
// PodName/ResolvedClass because its LLM only used respond_to_user.
func stampBundleSessionsReady(ctx context.Context, t *testing.T, c client.Client) {
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
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

			// PodName + Sandbox: the ToolCall controller resolves its exec
			// transport from resolved.session.Status.Sandbox (fail-closed —
			// see toolcall.Reconciler.executorFor), not PodName directly.
			// The production SpiceboxSession controller would set PodName
			// from podspec.PodNameFor(s) → "<name>-pod" and Sandbox from the
			// pod kind's Runtime.Ensure; mirror both here.
			if s.Status.PodName == "" {
				s.Status.PodName = s.Name + "-pod"
				s.Status.Sandbox = &spiceboxv1alpha1.SandboxHandle{
					Kind: pod.KindName,
					Ref:  s.Namespace + "/" + s.Status.PodName,
				}
				dirty = true
			}

			// ResolvedClass: the ToolCall controller reads
			// resolved.session.Status.ResolvedClass to find the class
			// tool catalog. EffectiveToolspecs is what
			// validateToolspec.gatherCandidates iterates to pick the
			// toolspec list for the call. Resolve the class lazily so
			// the helper doesn't need to know the class spec ahead of
			// time; mirror what the SpiceboxSession controller would
			// stamp on first reconcile.
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
				// Effective toolspec set = the SpiceboxSession.spec.toolspecs
				// when set, else the resolved class's spec.toolspecs.
				// (Mirrors computeEffectiveToolspecs.)
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

			// Ready=True so the AgentSession's BundlesReady gate flips
			// True and the ToolCall validate() doesn't bounce on
			// SessionNotActive.
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
				// Conflict on a concurrent reconcile is benign; the next
				// tick re-tries with a fresh ResourceVersion.
				t.Logf("stampBundleSessionsReady: update %s/%s: %v",
					s.Namespace, s.Name, err)
			}
		}
	}
}

// programGitPodResponse polls for the git bundle's SpiceboxSession,
// then programs fakeExec with a canned response for the pod's "sandbox"
// container. Mirrors stampBundleSessionsReady's structure (poll until
// the object appears, then act). Returns once the response is
// programmed, or ctx is canceled.
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
				// stampBundleSessionsReady hasn't filled PodName yet;
				// loop back and try again on the next tick.
				continue
			}
			key := s.Namespace + "/" + s.Status.PodName + ":sandbox"
			h.FakeExec().Program(key, fake.Response{
				Stdout:   []byte("nothing to commit, working tree clean\n"),
				ExitCode: 0,
			})
			return
		}
	}
}
