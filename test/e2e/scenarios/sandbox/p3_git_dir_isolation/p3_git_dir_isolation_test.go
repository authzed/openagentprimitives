//go:build e2e

package p3_git_dir_isolation_test

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
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestP3_GitDirIsolation is Phase C's headline end-to-end proof: two
// bundles share a workspace PVC, but only the "gitlike" SpiceboxClass
// declares EnvDefaults + PrivateVolumes. The pod-spec builder must apply
// both to the gitlike bundle's pod ONLY — leaving the "codelike" bundle's
// pod free of the AP_PRIVATE_DIR env and the /var/ap-git mount while
// still mounting the same shared workspace claim.
//
// The harness has no real Pod scheduler, so the test materializes each
// bundle's pod spec directly via podspec.Build(session, class) once the
// AgentSession controller has created the bundle SpiceboxSessions. That
// path is the production code path the SpiceboxSession controller calls
// before Apply — proving the end-to-end wiring from CR → AgentSession
// reconciler → SpiceboxSession (+ resolved class) → pod spec.
//
// The LLM tool-use round-trip (gitlike_git → codelike_cat → respond_to_user)
// is there to drive the AgentSession through the full session lifecycle so
// the pods + sessions actually get created; the headline isolation assertions
// happen on the materialized pod specs after the reply.
func TestP3_GitDirIsolation(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		WithToolCallController: true,
		WorkspaceStorageClass:  "rwx-test",
		ExtraManifests:         []string{string(manifests)},
		DefaultTimeout:         20 * time.Second,
	})

	stamperDone := make(chan struct{})
	programDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		<-stamperDone
		<-programDone
	})

	// 1. Stamp both Toolspecs Valid=True so AgentClass binding-coverage
	// accepts ts-gitlike and ts-codelike. The harness doesn't wire the
	// SpiceboxToolspec controller (same as p1/p2 scenarios).
	stampToolspecsValid(t, ctx, h.K8s, "ts-gitlike", "ts-codelike")

	// 2. Background loop: stamp PodName + ResolvedClass + EffectiveToolspecs
	// and flip Ready=True on each bundle SpiceboxSession as it appears.
	go func() {
		defer close(stamperDone)
		stampBundleSessionsReady(ctx, t, h.K8s)
	}()

	// 3. Program canned exec responses for both bundle pods. The fake
	// programs are persistent map entries (not queues), so one Program
	// per pod is enough for any number of tool_use calls against it.
	// Use distinct stdout payloads so the per-pod programming is
	// observable in the fake exec call log.
	go func() {
		defer close(programDone)
		programBundlePodResponses(ctx, t, h)
	}()

	// 4. LLM script. The "_<class-tool>" suffix on each tool name is the
	// class-tool catalog entry (git/cat), not the toolkit. Two-step round-
	// trip: mint op_id → gitlike_git → codelike_cat → respond_to_user.
	var capturedOpID string
	h.LLM.OnUserMessage("isolate then verify").Reply(e2e.ToolUse("new_operation", map[string]any{
		"description": "drive both bundles to prove credential isolation",
	}))
	h.LLM.OnToolResult("new_operation", e2e.ResultMatches(func(got any) bool {
		if m, ok := got.(map[string]any); ok {
			if id, ok := m["operation_id"].(string); ok {
				capturedOpID = id
			}
		}
		return true
	})).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("gitlike_git", map[string]any{
			"operation_id": capturedOpID,
			"_reason":      "write markers in the gitlike bundle",
			"args":         []string{"status"},
		})}
	})
	h.LLM.OnToolResult("gitlike_git", e2e.AnyResult()).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("codelike_cat", map[string]any{
			"operation_id": capturedOpID,
			"_reason":      "verify isolation from the codelike bundle",
			"args":         []string{},
		})}
	})
	h.LLM.OnToolResult("codelike_cat", e2e.AnyResult()).Reply(
		e2e.RespondToUser("verified: isolation holds"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.WaitForAgentClassValid("ac-isolation", 30*time.Second)
	h.SendUserMessage("isolate then verify")
	h.ExpectAgentReply(e2e.Contains("verified: isolation holds"))

	// 5. The headline assertions: materialize each bundle's pod spec via
	// the production podspec.Build path and prove the per-class isolation
	// invariants. The stamper helper populated session.Status.ResolvedClass
	// from each SpiceboxClass; podspec.Build expects the SpiceboxClassSpec
	// directly (not the wrapper), so dereference here.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(ctx, &sessions, client.InNamespace("default")),
		"list AgentSessions")
	require.NotEmpty(t, sessions.Items, "channelsd pipeline should have created an AgentSession")
	sess := sessions.Items[0]

	// (a) Same shared workspace PVC, owned by the AgentSession (Phase B's
	// invariant — restated here so a regression in shared-claim wiring
	// fails this scenario instead of bleeding through silently).
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{
		Namespace: "default", Name: sess.Name + "-workspace",
	}, &pvc), "workspace PVC should exist")
	require.Len(t, pvc.OwnerReferences, 1, "workspace PVC must be owned by AgentSession")
	assert.Equal(t, sess.Name, pvc.OwnerReferences[0].Name, "workspace PVC owner")

	gitSess := getBundleSession(t, ctx, h.K8s, sess.Name+"-gitlike")
	codeSess := getBundleSession(t, ctx, h.K8s, sess.Name+"-codelike")
	require.NotNil(t, gitSess.Status.ResolvedClass, "gitlike session must have ResolvedClass stamped")
	require.NotNil(t, codeSess.Status.ResolvedClass, "codelike session must have ResolvedClass stamped")

	// (b) Materialize each pod spec via the production builder.
	gitPod, err := podspec.Build(gitSess, *gitSess.Status.ResolvedClass)
	require.NoError(t, err, "build gitlike pod spec")
	codePod, err := podspec.Build(codeSess, *codeSess.Status.ResolvedClass)
	require.NoError(t, err, "build codelike pod spec")

	// (c) Gitlike pod: AP_PRIVATE_DIR is set, ap-git emptyDir is mounted
	// at /var/ap-git, workspace PVC is mounted.
	assert.Equal(t, "/var/ap-git", envValue(gitPod, "AP_PRIVATE_DIR"),
		"gitlike pod must carry AP_PRIVATE_DIR from class.EnvDefaults")
	assert.True(t, hasVolumeMount(gitPod, "ap-git", "/var/ap-git"),
		"gitlike pod must mount the oap-git private emptyDir at /var/ap-git")
	assert.True(t, hasEmptyDirVolume(gitPod, "ap-git"),
		"gitlike pod must declare ap-git as emptyDir")
	assert.True(t, hasWorkspaceMount(gitPod, sess.Name+"-workspace"),
		"gitlike pod must mount the shared workspace PVC")

	// (d) Codelike pod: AP_PRIVATE_DIR is NOT set, /var/ap-git is NOT
	// mounted, ap-git volume is NOT declared — but workspace PVC IS
	// mounted (the shared-claim path is independent of per-bundle
	// EnvDefaults/PrivateVolumes).
	assert.Empty(t, envValue(codePod, "AP_PRIVATE_DIR"),
		"codelike pod must NOT carry AP_PRIVATE_DIR (class.EnvDefaults unset)")
	assert.False(t, hasMountPath(codePod, "/var/ap-git"),
		"codelike pod must NOT mount /var/ap-git (class.PrivateVolumes unset)")
	assert.False(t, hasVolumeNamed(codePod, "ap-git"),
		"codelike pod must NOT declare an ap-git volume")
	assert.True(t, hasWorkspaceMount(codePod, sess.Name+"-workspace"),
		"codelike pod must mount the shared workspace PVC (shared mount must still work)")

	// (e) Sanity: the round-trip actually exercised both bundle pods'
	// exec endpoints. Filter by pod name so a regression that routes
	// both tool_uses through one pod (or none) fails here.
	calls := h.FakeExec().Calls()
	require.GreaterOrEqual(t, len(calls), 2,
		"expected at least 2 fake exec calls (gitlike + codelike); got %d", len(calls))
	assert.GreaterOrEqual(t, len(filterCallsByPod(calls, sess.Name+"-gitlike-pod")), 1,
		"expected at least one exec call against the gitlike pod")
	assert.GreaterOrEqual(t, len(filterCallsByPod(calls, sess.Name+"-codelike-pod")), 1,
		"expected at least one exec call against the codelike pod")

	h.AssertAllRulesConsumed()
}

// envValue returns the value of envName on the pod's spicebox container
// (which is always Containers[0] in the builder's output), or "" if not set.
func envValue(pod *corev1.Pod, envName string) string {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == envName {
			return e.Value
		}
	}
	return ""
}

// hasVolumeMount returns true when the spicebox container mounts the
// named volume at the expected path.
func hasVolumeMount(pod *corev1.Pod, name, path string) bool {
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == name && m.MountPath == path {
			return true
		}
	}
	return false
}

// hasMountPath returns true when ANY VolumeMount on the spicebox
// container has the given path. Used for negative assertions where the
// volume's name is not known to the caller.
func hasMountPath(pod *corev1.Pod, path string) bool {
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.MountPath == path {
			return true
		}
	}
	return false
}

// hasEmptyDirVolume returns true when the pod declares an EmptyDir
// volume with the given name.
func hasEmptyDirVolume(pod *corev1.Pod, name string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == name && v.EmptyDir != nil {
			return true
		}
	}
	return false
}

// hasVolumeNamed returns true when the pod's Volumes contain an entry
// with the given name regardless of type. Used for negative assertions.
func hasVolumeNamed(pod *corev1.Pod, name string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// hasWorkspaceMount returns true when the pod mounts the shared
// workspace PVC by claim name. Mirrors the builder's hard-coded volume
// name ("workspace") + mount path (/workspace).
func hasWorkspaceMount(pod *corev1.Pod, claim string) bool {
	var foundVolume bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == "workspace" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			foundVolume = true
			break
		}
	}
	if !foundVolume {
		return false
	}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "workspace" && m.MountPath == "/workspace" {
			return true
		}
	}
	return false
}

// filterCallsByPod returns the subset of fake.Calls whose Pod matches pod.
// Lifted from p2_end_to_end_git_claude_git.
func filterCallsByPod(calls []fake.Call, pod string) []fake.Call {
	out := make([]fake.Call, 0, len(calls))
	for _, c := range calls {
		if c.Pod == pod {
			out = append(out, c)
		}
	}
	return out
}

// getBundleSession Gets a SpiceboxSession by name in the default
// namespace, failing the test if it's missing.
func getBundleSession(t *testing.T, ctx context.Context, c client.Client, name string) *spiceboxv1alpha1.SpiceboxSession {
	t.Helper()
	var s spiceboxv1alpha1.SpiceboxSession
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &s),
		"get SpiceboxSession %q", name)
	return &s
}

// stampToolspecsValid sets Status.Conditions[Valid]=True on each named
// SpiceboxToolspec. Lifted from p1/p2 — promotion into a shared helper
// is a deliberate follow-up once the scenario shape settles.
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
// namespace and stamps PodName, ResolvedClass, EffectiveToolspecs, then
// Ready=True. Idempotent — already-stamped fields are not rewritten on
// subsequent ticks. Lifted from p1_defaultenv.
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
				// Conflict on concurrent reconcile is benign; the next tick
				// re-tries with a fresh ResourceVersion.
				t.Logf("stampBundleSessionsReady: update %s/%s: %v",
					s.Namespace, s.Name, err)
			}
		}
	}
}

// programBundlePodResponses polls for both bundle SpiceboxSessions, then
// programs a canned exec response on each pod's "sandbox" container.
// Program is persistent (the fake stores a map, not a queue), so one
// Program per pod is enough for any number of tool_use calls.
func programBundlePodResponses(ctx context.Context, t *testing.T, h *e2e.Harness) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	programmed := map[string]bool{} // bundle name → done
	want := map[string][]byte{
		"gitlike":  []byte("wrote markers (gitlike)\n"),
		"codelike": []byte("ISOLATION_OK (codelike)\n"),
	}
	for {
		if len(programmed) == len(want) {
			return
		}
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
			t.Logf("programBundlePodResponses: list: %v", err)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			bundle := s.Labels["agentprimitives.authzed.com/agentbundle"]
			if bundle == "" || programmed[bundle] {
				continue
			}
			stdout, ok := want[bundle]
			if !ok {
				continue
			}
			if s.Status.PodName == "" {
				continue
			}
			key := s.Namespace + "/" + s.Status.PodName + ":sandbox"
			h.FakeExec().Program(key, fake.Response{
				Stdout:   stdout,
				ExitCode: 0,
			})
			programmed[bundle] = true
		}
	}
}
