//go:build e2e

// Package passthrough_sandbox_test is the end-to-end scenario proving that an
// identityMode=userPassthrough AgentSession dispatches a SANDBOX CLI ToolCall
// whose credential resolves from the per-session materialized Secret in the
// SESSION namespace — so the ToolCall passes ValidateCredentialSourceNamespaces
// (Source.Namespace == ToolCall.Namespace) rather than failing
// ReasonCredentialSourceForbidden.
//
// Why this scenario exists — the fidelity gap it closes:
//
// The cross-namespace passthrough-credential bug shipped because the e2e
// harness's buildSandboxTools hardcoded the agent-identity credential path
// (RuntimeIdentityFromAgentIdentity), so a sandbox ToolCall's credential
// source was never driven through the userPassthrough branch. The existing
// passthrough e2e scenarios are MCP-only (credentials become HTTP headers, no
// ToolCall), so the cross-namespace ToolCall validation was never exercised.
// buildSandboxTools now branches on identityMode through the SAME
// runner.BundleRuntimeIdentity helper internal/cmd/runner/main.go uses; this scenario
// drives that branch end-to-end.
//
// What it proves end-to-end:
//
//  1. A type:static user credential ("git-token") is pre-linked at runtime via
//     useridentity.PutToken. The git toolkit declares GIT_TOKEN sensitive with
//     credential=git-token, so the passthrough gate's RequiredCredentials finds
//     "git-token" required, sees it linked, and UNPARKS the session — creating
//     the SessionUserIdentity and materializing <session>-passthrough-creds in
//     the session namespace.
//
//  2. The runner synthesizes the git sandbox tool via the userPassthrough
//     branch, so its credential descriptor points at
//     {static, <sessionNs>, <session>-passthrough-creds, key=git-token}.
//
//  3. The git_git sandbox ToolCall dispatches: the ToolCall controller's
//     ValidateCredentialSourceNamespaces passes (Source.Namespace ==
//     ToolCall.Namespace), the broker resolves the credential from the
//     per-session Secret, and the call reaches the fake exec + FinishedAt with
//     exit 0 — NOT a terminal Failed/ReasonCredentialSourceForbidden.
//
// Conceptually this would have caught the original bug: had the descriptor
// source pointed at the identities namespace (the pre-fix behavior), the
// ToolCall would fail validation, the sandbox exec would never run, and both
// the fake-exec assertion and the ToolCall-status assertion below would fail.
//
// No real names: alice / example.com are fictional per AGENTS.md.
package passthrough_sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// starterEmail is the human the test impersonates. Canonicalized into a
	// SpiceDB subject via identity.Principal.Canonical(); the same canonical
	// must be passed to PutToken so the passthrough gate's
	// NameForSubject(subject) finds the pre-linked UserIdentity (otherwise the
	// session parks in AwaitingCredentials instead of running).
	starterEmail = "alice@example.com"

	// gitTokenCredential is the credential name the git toolkit declares for
	// its sensitive GIT_TOKEN env (toolkits/git.yaml). RequiredCredentials for
	// the git bundle yields exactly this name, so it is what we pre-link.
	gitTokenCredential = "git-token"

	// preLinkedToken is the static token value seeded before the session runs.
	preLinkedToken = "pt-sandbox-token-9f8e7d6c5b4a"
)

// TestPassthroughSandboxCredentialReachesToolCall drives a userPassthrough
// session through a git sandbox ToolCall and asserts the credential resolves
// from the session-namespace per-session Secret (not the identities
// namespace), so the call dispatches instead of failing
// ReasonCredentialSourceForbidden.
func TestPassthroughSandboxCredentialReachesToolCall(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		WithToolCallController: true,
		WorkspaceStorageClass:  "rwx-test",
		DefaultUser:            starterEmail,
		ExtraManifests:         []string{string(manifests)},
	})

	// done channel joins the background stamper so the test doesn't return
	// while it is still running (p1_defaultenv's join pattern).
	stamperDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		<-stamperDone
	})

	// envtest doesn't auto-create namespaces; PutToken's master Secret Create
	// lands in the identities namespace, and the passthrough gate reads it
	// there to materialize the per-session Secret.
	createIdentitiesNamespace(t, ctx, h.K8s)

	// Pre-link the static "git-token" credential for the starter BEFORE
	// SendUserMessage. The Subject must match the canonical the channelsd
	// pipeline stamps on the session (derived from DefaultUser).
	starterCanon, err := identity.EmailReference(starterEmail).Canonical()
	require.NoError(t, err)
	starterCanonical := starterCanon.Subject()
	require.NoError(t, useridentity.PutToken(ctx, h.K8s, useridentity.PutTokenRequest{
		Subject:        starterCanonical,
		CredentialName: gitTokenCredential,
		Token:          preLinkedToken,
	}), "pre-link git-token credential")

	// Stamp ts-git Valid=True: validateBundles rejects a toolspec that has no
	// Valid=True condition, and the harness doesn't run the SpiceboxToolspec
	// controller that would stamp it. No manual reconcile poke is needed —
	// the AgentClass controller Watches SpiceboxToolspec
	// (mapToolspecToClasses re-enqueues every class whose
	// toolBundles[].toolspecs names it, identityMode-independent), so this
	// status write re-triggers the ac-pt-git reconcile that lands Valid=True.
	stampToolspecsValid(t, ctx, h.K8s, "ts-git")

	// Background loop: stamp every bundle SpiceboxSession Ready=True +
	// PodName + ResolvedClass the moment it appears (the harness doesn't run
	// the SpiceboxSession controller or a Pod scheduler). For the git bundle it
	// also programs the fake exec response BEFORE stamping PodName, so the
	// canned response is registered before the ToolCall controller can read
	// PodName and dispatch the exec — eliminating the program-vs-exec race.
	go func() {
		defer close(stamperDone)
		stampBundleSessionsReady(ctx, t, h)
	}()

	// LLM script (identical shape to p1_defaultenv):
	//   "status" → new_operation (mints an operation_id)
	//   tool_result(new_operation) → git_git(args=[status], captured op id)
	//   tool_result(git_git) → respond_to_user("clean")
	//   tool_result(respond_to_user) → EndTurn
	var capturedOpID string
	h.LLM.OnUserMessage("status").Reply(e2e.ToolUse("new_operation", map[string]any{
		"description": "show git status",
	}))
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
			"_reason":      "see what changed",
			"args":         []string{"status"},
		})}
	})
	h.LLM.OnToolResult("git_git", e2e.AnyResult()).Reply(e2e.RespondToUser("clean"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.WaitForAgentClassValid("ac-pt-git", 30*time.Second)
	h.SendUserMessage("status")
	h.ExpectAgentReply(e2e.Contains("clean"))

	// === Assertion 1: the sandbox exec actually ran. ===
	// If the git_git ToolCall had failed ValidateCredentialSourceNamespaces,
	// it would be set Failed BEFORE reaching exec, so the fake exec would
	// never see a sandbox call. Its presence proves credential validation
	// passed and the call dispatched.
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
	assert.Equal(t, preLinkedToken, gitCall.Request.Env["GIT_TOKEN"],
		"the pre-linked git-token must be injected into the exec env as GIT_TOKEN via the per-session passthrough Secret")

	// === Assertion 2: the git ToolCall is VALIDATED + dispatched with a
	// session-namespace credential source — NOT failed-forbidden. ===
	tc := requireFinishedCredentialedToolCall(t, ctx, h.K8s, "default")

	require.Len(t, tc.Spec.Credentials, 1, "the git ToolCall must carry exactly one resolved credential descriptor")
	src := tc.Spec.Credentials[0].Source
	assert.Equal(t, "static", src.Type, "passthrough static credential descriptor")
	// The whole point of the cross-namespace fix: the source must resolve
	// WITHIN the ToolCall's own namespace, not the identities namespace.
	assert.Equal(t, tc.Namespace, src.Namespace,
		"credential source namespace must equal the ToolCall namespace (cross-namespace source would be refused)")
	assert.NotEqual(t, spiceboxv1alpha1.IdentitiesNamespace, src.Namespace,
		"credential source must NOT point at the identities namespace (the pre-fix behavior that failed validation)")

	owner := agentSessionOwner(tc)
	require.NotEmpty(t, owner, "the git ToolCall must be owned by an AgentSession")
	assert.Equal(t, spiceboxv1alpha1.PassthroughCredentialSecretName(owner), src.Name,
		"credential source must be this session's per-session passthrough Secret")
	assert.Equal(t, gitTokenCredential, src.Key,
		"credential source key must be the credential name the per-session Secret is keyed by")

	// Must not be terminally failed for a forbidden credential source.
	assert.False(t, isFailedForbidden(tc),
		"git ToolCall must NOT be Failed=True/ReasonCredentialSourceForbidden; conditions=%+v", tc.Status.Conditions)
	require.NotNil(t, tc.Status.ExitCode, "a dispatched ToolCall must record an exit code")
	assert.Equal(t, int32(0), *tc.Status.ExitCode, "the fake git exec returned exit 0")

	h.AssertAllRulesConsumed()
}

// ----- assertion helpers -------------------------------------------------

// requireFinishedCredentialedToolCall polls for the single ToolCall in ns that
// carries a credential descriptor (only the git sandbox call does) and has
// reached FinishedAt. After ExpectAgentReply the call is already terminal, but
// a short poll absorbs any status-write propagation lag.
func requireFinishedCredentialedToolCall(t *testing.T, ctx context.Context, c client.Client, ns string) *spiceboxv1alpha1.ToolCall {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last *spiceboxv1alpha1.ToolCall
	for time.Now().Before(deadline) {
		var list spiceboxv1alpha1.ToolCallList
		if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
			if ctx.Err() != nil {
				t.Fatalf("requireFinishedCredentialedToolCall: list canceled: %v", err)
			}
			t.Logf("requireFinishedCredentialedToolCall: list: %v", err)
			time.Sleep(150 * time.Millisecond)
			continue
		}
		for i := range list.Items {
			tc := &list.Items[i]
			if len(tc.Spec.Credentials) == 0 {
				continue
			}
			last = tc
			if tc.Status.FinishedAt != nil {
				return tc
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if last != nil {
		t.Fatalf("requireFinishedCredentialedToolCall: credentialed ToolCall %s/%s never reached FinishedAt; conditions=%+v",
			last.Namespace, last.Name, last.Status.Conditions)
	}
	t.Fatalf("requireFinishedCredentialedToolCall: no credentialed ToolCall observed in %q within 15s "+
		"(a passthrough sandbox call should stamp a per-session credential descriptor)", ns)
	return nil
}

// agentSessionOwner returns the name of the AgentSession that owns the ToolCall.
func agentSessionOwner(tc *spiceboxv1alpha1.ToolCall) string {
	for _, o := range tc.OwnerReferences {
		if o.Kind == "AgentSession" {
			return o.Name
		}
	}
	return ""
}

// isFailedForbidden reports whether the ToolCall is terminally failed because
// its credential source was refused (the bug this scenario guards against).
func isFailedForbidden(tc *spiceboxv1alpha1.ToolCall) bool {
	cond := meta.FindStatusCondition(tc.Status.Conditions, spiceboxv1alpha1.ToolCallConditionFailed)
	return cond != nil &&
		cond.Status == metav1.ConditionTrue &&
		cond.Reason == spiceboxv1alpha1.ReasonCredentialSourceForbidden
}

// createIdentitiesNamespace creates the agentprimitives-identities namespace.
// envtest doesn't auto-create namespaces; PutToken's master Secret lands there.
func createIdentitiesNamespace(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.IdentitiesNamespace},
	}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %s namespace: %v", spiceboxv1alpha1.IdentitiesNamespace, err)
	}
}

// ----- harness-stamping helpers (mirrored from p1_defaultenv) -------------
//
// The e2e harness does not run the SpiceboxToolspec / SpiceboxSession
// controllers or a Pod scheduler, so these helpers stamp what those would
// produce. Inlined per-scenario to avoid cross-package coupling (the same
// promotion-to-shared-helper follow-up noted in p1_defaultenv applies).

// stampToolspecsValid sets Status.Conditions[Valid]=True on each named
// SpiceboxToolspec so the AgentClass binding-coverage check passes.
func stampToolspecsValid(t *testing.T, ctx context.Context, c client.Client, names ...string) {
	t.Helper()
	for _, name := range names {
		var ts spiceboxv1alpha1.SpiceboxToolspec
		require.NoError(t, c.Get(ctx, client.ObjectKey{Name: name}, &ts), "get toolspec %q", name)
		ts.Status.Conditions = []metav1.Condition{{
			Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
			Status:             metav1.ConditionTrue,
			Reason:             "Resolved",
			LastTransitionTime: metav1.Now(),
		}}
		require.NoError(t, c.Status().Update(ctx, &ts), "stamp toolspec %q Valid=True", name)
	}
}

// stampBundleSessionsReady polls for SpiceboxSessions in the default namespace
// and stamps any without a Ready=True condition, also filling PodName +
// ResolvedClass + EffectiveToolspecs (the ToolCall controller reads all of
// these in validate()). For the git bundle it programs the fake exec response
// for the (deterministic) pod name BEFORE stamping PodName, so the response is
// always registered before the ToolCall controller can read PodName and run
// the exec. Runs until ctx is canceled; idempotent.
func stampBundleSessionsReady(ctx context.Context, t *testing.T, h *e2e.Harness) {
	c := h.K8s
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
				podName := s.Name + "-pod"
				// Program the fake exec for the git pod's sandbox container
				// BEFORE PodName becomes visible, so the canned response is
				// registered ahead of any dispatch. Idempotent (re-program is
				// a map overwrite of the same value).
				if s.Labels["agentprimitives.authzed.com/agentbundle"] == "git" {
					key := s.Namespace + "/" + podName + ":sandbox"
					h.FakeExec().Program(key, fake.Response{
						Stdout:   []byte("nothing to commit, working tree clean\n"),
						ExitCode: 0,
					})
				}
				s.Status.PodName = podName
				s.Status.Sandbox = &spiceboxv1alpha1.SandboxHandle{
					Kind: pod.KindName,
					Ref:  s.Namespace + "/" + podName,
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
				s.Status.ResolvedClass = cls.Spec.DeepCopy()
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
				t.Logf("stampBundleSessionsReady: update %s/%s: %v", s.Namespace, s.Name, err)
			}
		}
	}
}
