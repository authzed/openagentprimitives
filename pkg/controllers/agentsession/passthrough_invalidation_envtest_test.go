//go:build integration

// pkg/controllers/agentsession/passthrough_invalidation_envtest_test.go
//
// Integration test proving the operator emits a per-session `credential`
// invalidation on the oap.revocation bus when a userPassthrough session's
// projected credential changes (replace/remove), and re-projects the
// per-session Secret to match. Companion to:
//   - passthrough_envtest_test.go: the park -> un-park lifecycle (envtest,
//     package agentsession_test, drives a live manager + watches).
//   - passthrough_credhash_test.go: unit coverage of the diff+emit helper
//     (applyPassthroughCredInvalidation) in isolation, including fakeBus.
//
// This file is package agentsession (not agentsession_test) so it can extend
// and reuse the unexported fakeBus type declared in passthrough_credhash_test.go
// (an untagged file, so it compiles under both `go test` and `-tags=integration`).
// It drives the REAL top-level Reconciler.Reconcile — by hand, request by
// request, mirroring the direct-Reconcile pattern used throughout this
// package's other //go:build integration tests (e.g. controller_test.go,
// identitychoice_envtest_test.go) — against the shared envtest apiserver from
// sharedenv_test.go's TestMain.
//
// Two required static credentials (cred-alpha, cred-beta) are used rather
// than one: the per-session Secret is re-projected only when SOMETHING is
// still projectable (passthrough.go skips materializePassthroughCredentials
// for an empty static set, leaving the previous Secret's keys in place), and
// removing a session's ONLY required credential from its UserIdentity parks
// the session (AwaitingCredentials) rather than shrinking the projected set.
// Keeping cred-beta required throughout lets step 4 shrink `required` down to
// just cred-beta (by dropping the cred-alpha MCPServer from the AgentClass)
// and actually exercise diffCredHashes' "removed" branch end-to-end, with the
// per-session Secret provably losing the cred-alpha key.
package agentsession

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"

	// Registers MCP + CLI + toolspec authkind so RequiredCredentials can
	// resolve MCPServer targets.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
)

// invalidationNoopRunnerFactory is a minimal RunnerFactory — this exercise
// focuses on the credential-invalidation emit + re-projection, not pod
// lifecycle (mirrors passthroughNoopRunnerFactory in passthrough_envtest_test.go).
type invalidationNoopRunnerFactory struct{}

func (invalidationNoopRunnerFactory) Start(_ context.Context, _ *spiceboxv1alpha1.AgentSession, _ *spiceboxv1alpha1.AgentClass, _ StartOpts) error {
	return nil
}
func (invalidationNoopRunnerFactory) Stop(_ context.Context, _ *spiceboxv1alpha1.AgentSession) error {
	return nil
}

// ObservedName returns "" — this fake creates no workload for the
// reconciler to observe.
func (invalidationNoopRunnerFactory) ObservedName(_ *spiceboxv1alpha1.AgentSession) string {
	return ""
}

// ensureIdentitiesNamespace creates the agentprimitives-identities namespace,
// tolerating AlreadyExists. testenv.Reset does not delete namespaces (envtest
// has no namespace GC), so a test that creates one must tolerate it already
// existing from an earlier test in the same shared apiserver.
func ensureIdentitiesNamespace(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.IdentitiesNamespace}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create identities namespace")
	}
}

// passthroughSecretValue reads key from the named per-session Secret,
// returning ("", false) if the Secret or key does not exist. Distinguishing
// "key absent" from "key present but empty" matters for the step-4 removal
// assertion.
func passthroughSecretValue(t *testing.T, ctx context.Context, c client.Client, ns, name, key string) (string, bool) {
	t.Helper()
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sec); err != nil {
		require.True(t, apierrors.IsNotFound(err), "unexpected error reading projected Secret %s/%s: %v", ns, name, err)
		return "", false
	}
	if v, ok := sec.StringData[key]; ok {
		return v, true
	}
	v, ok := sec.Data[key]
	return string(v), ok
}

// TestPassthroughCredentialInvalidation_Envtest drives a real userPassthrough
// AgentSession reconcile loop through: prime (no emit) -> replace one
// credential's master value (one emit, re-projected) -> remove one
// credential's requirement (one more emit, key dropped from the projected
// Secret). It proves the wiring from Task 4-8 (hash diff -> RevokePublisher
// -> ap.revocation "credential" envelope) end-to-end through the operator's
// actual Reconcile, not just the unit-level applyPassthroughCredInvalidation
// helper covered by passthrough_credhash_test.go.
func TestPassthroughCredentialInvalidation_Envtest(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	ensureIdentitiesNamespace(t, ctx, env.Client)

	const subject = "user:passthrough-invalidation@example.com"
	const credAlpha = "cred-alpha"
	const credBeta = "cred-beta"

	// AgentClass: static userPassthrough, references two credentialed
	// MCPServers so step 4 can shrink `required` from two down to one while
	// something projectable remains.
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pt-inv-cls", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-opus-4-7",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    50,
				MaxTokens:   100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
			MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
				{Name: "mcp-alpha", Ref: "pt-inv-mcp-alpha"},
				{Name: "mcp-beta", Ref: "pt-inv-mcp-beta"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	ac.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentClassConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonAllReferencesResolve,
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, ac), "stamp AgentClass Valid=True")

	mcpAlpha := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "pt-inv-mcp-alpha", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp-alpha.example.com/mcp"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Credential: credAlpha},
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcpAlpha), "create MCPServer mcp-alpha")

	mcpBeta := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "pt-inv-mcp-beta", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp-beta.example.com/mcp"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Credential: credBeta},
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcpBeta), "create MCPServer mcp-beta")

	// Master Secrets (identities namespace) + UserIdentity: both credentials
	// type=static. cred-alpha starts at "tok-1"; cred-beta is a stable control
	// value that must survive every step untouched.
	masterAlphaName := useridentity.NameForSubject(subject) + "-" + credAlpha
	masterAlpha := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: masterAlphaName, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data:       map[string][]byte{"token": []byte("tok-1")},
	}
	require.NoError(t, env.Client.Create(ctx, masterAlpha), "create master Secret for cred-alpha")

	masterBetaName := useridentity.NameForSubject(subject) + "-" + credBeta
	masterBeta := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: masterBetaName, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data:       map[string][]byte{"token": []byte("tok-beta-const")},
	}
	require.NoError(t, env.Client.Create(ctx, masterBeta), "create master Secret for cred-beta")

	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(subject)},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: subject,
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: credAlpha,
					Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: masterAlphaName, Key: "token"},
					},
				},
				{
					Name: credBeta,
					Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: masterBetaName, Key: "token"},
					},
				},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ui), "create UserIdentity")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pt-inv-sess",
			Namespace: "default",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: subject,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "pt-inv-cls",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "help me"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	bus := &fakeBus{}
	r := &Reconciler{
		Client:          env.Client,
		APIReader:       env.Client,
		Tokens:          tokens.NewRegistry(),
		Memory:          memory.NewLocal(inmem.NewBackend()),
		RunnerFactory:   invalidationNoopRunnerFactory{},
		RevokePublisher: revocation.NewPublisher(bus),
	}

	key := client.ObjectKeyFromObject(sess)
	reconcileN := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			require.NoError(t, err, "reconcile %d/%d", i+1, n)
		}
	}

	projectedName := spiceboxv1alpha1.PassthroughCredentialSecretName(sess.Name)

	// ---- Step 1: prime — first reconcile(s) project both credentials, ------
	// ---- no emit yet (nothing to invalidate on first sight). ---------------
	reconcileN(4) // mirrors bootstrapSession's finalizer/RBAC/pod settle count

	alphaVal, ok := passthroughSecretValue(t, ctx, env.Client, "default", projectedName, credAlpha)
	require.True(t, ok, "cred-alpha must be projected after prime")
	assert.Equal(t, "tok-1", alphaVal, "initial projected cred-alpha value")
	betaVal, ok := passthroughSecretValue(t, ctx, env.Client, "default", projectedName, credBeta)
	require.True(t, ok, "cred-beta must be projected after prime")
	assert.Equal(t, "tok-beta-const", betaVal, "initial projected cred-beta value")

	var afterPrime spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &afterPrime), "get session after prime")
	require.NotNil(t, afterPrime.Status.PassthroughCredHashes, "status.passthroughCredHashes must be set after prime")
	assert.Contains(t, afterPrime.Status.PassthroughCredHashes, credAlpha)
	assert.Contains(t, afterPrime.Status.PassthroughCredHashes, credBeta)
	assert.Empty(t, bus.envelopes, "prime must not emit an invalidation")

	// ---- Step 2: rotate cred-alpha's master Secret to "tok-2" and ----------
	// ---- reconcile once -> re-projected, hash advances, exactly ONE --------
	// ---- emitted envelope keyed on the per-session projected Secret. -------
	var freshMasterAlpha corev1.Secret
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: masterAlphaName}, &freshMasterAlpha),
		"get master Secret for cred-alpha before rotation")
	freshMasterAlpha.Data["token"] = []byte("tok-2")
	require.NoError(t, env.Client.Update(ctx, &freshMasterAlpha), "rotate master Secret for cred-alpha to tok-2")

	reconcileN(1)

	alphaVal, ok = passthroughSecretValue(t, ctx, env.Client, "default", projectedName, credAlpha)
	require.True(t, ok, "cred-alpha must still be projected after rotation")
	assert.Equal(t, "tok-2", alphaVal, "projected cred-alpha value must follow the rotated master")
	betaVal, ok = passthroughSecretValue(t, ctx, env.Client, "default", projectedName, credBeta)
	require.True(t, ok, "cred-beta must be untouched by the cred-alpha rotation")
	assert.Equal(t, "tok-beta-const", betaVal, "cred-beta value must not change on a cred-alpha rotation")

	var afterReplace spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &afterReplace), "get session after replace")
	assert.NotEqual(t, afterPrime.Status.PassthroughCredHashes[credAlpha], afterReplace.Status.PassthroughCredHashes[credAlpha],
		"status hash for cred-alpha must advance on rotation")
	assert.Equal(t, afterPrime.Status.PassthroughCredHashes[credBeta], afterReplace.Status.PassthroughCredHashes[credBeta],
		"status hash for cred-beta must be unaffected")

	require.Len(t, bus.envelopes, 1, "exactly one envelope must be emitted for the rotation")
	assert.Equal(t, channelevents.KindRevoked, bus.envelopes[0].Kind, "emitted envelope Kind")
	var replacePayload channelevents.RevokedPayload
	require.NoError(t, json.Unmarshal(bus.envelopes[0].Payload, &replacePayload), "decode RevokedPayload (same as revocation.RegisterSubscriber)")
	assert.Equal(t, credentialRevokeKind, replacePayload.Kind, "RevokedPayload.Kind")
	wantKey := sess.Namespace + "/" + projectedName
	assert.Equal(t, wantKey, replacePayload.Key, "RevokedPayload.Key must be <sessNs>/<PassthroughCredentialSecretName>")
	assert.Equal(t, "", replacePayload.Scope, "passthrough invalidations are emitted cluster-wide (scope=\"\")")

	// ---- Step 3: drop the cred-alpha MCPServer from the AgentClass so ------
	// ---- `required` shrinks to just cred-beta (WITHOUT emptying it, which ---
	// ---- would leave nothing projectable and so skip the re-projection ----
	// ---- this step asserts on). Reconcile once -> per-session Secret no ----
	// ---- longer carries the cred-alpha key; one more emitted envelope. -----
	var freshClass spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "pt-inv-cls"}, &freshClass),
		"get AgentClass before shrinking MCPServers")
	freshClass.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: "mcp-beta", Ref: "pt-inv-mcp-beta"},
	}
	require.NoError(t, env.Client.Update(ctx, &freshClass), "drop mcp-alpha requirement from AgentClass")

	reconcileN(1)

	_, ok = passthroughSecretValue(t, ctx, env.Client, "default", projectedName, credAlpha)
	assert.False(t, ok, "cred-alpha key must be gone from the projected Secret once it is no longer required")
	betaVal, ok = passthroughSecretValue(t, ctx, env.Client, "default", projectedName, credBeta)
	require.True(t, ok, "cred-beta must still be projected")
	assert.Equal(t, "tok-beta-const", betaVal, "cred-beta value unaffected by the cred-alpha removal")

	var afterRemoval spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &afterRemoval), "get session after removal")
	assert.NotContains(t, afterRemoval.Status.PassthroughCredHashes, credAlpha,
		"status.passthroughCredHashes must drop cred-alpha once it is no longer required")
	assert.Contains(t, afterRemoval.Status.PassthroughCredHashes, credBeta,
		"status.passthroughCredHashes must retain cred-beta")

	require.Len(t, bus.envelopes, 2, "the removal must emit exactly one more envelope (2 total)")
	assert.Equal(t, channelevents.KindRevoked, bus.envelopes[1].Kind, "second emitted envelope Kind")
	var removePayload channelevents.RevokedPayload
	require.NoError(t, json.Unmarshal(bus.envelopes[1].Payload, &removePayload), "decode second RevokedPayload")
	assert.Equal(t, credentialRevokeKind, removePayload.Kind, "second RevokedPayload.Kind")
	assert.Equal(t, wantKey, removePayload.Key, "second RevokedPayload.Key is the same per-session Secret key")
	assert.Equal(t, "", removePayload.Scope, "second invalidation is also cluster-wide (scope=\"\")")
}
