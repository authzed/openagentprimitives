package toolcall

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
)

// fakeCheckCall records one CheckUseToken invocation, for asserting exactly
// what the checker was asked (the ns/name resource, the credID subject, and
// the presented value hash — never the raw token value).
type fakeCheckCall struct {
	ns, name, credID, presented string
	fullyConsistent             bool
}

// fakeTokenChecker is a scripted TokenChecker: every call returns the same
// (allowed, err) pair and is recorded so tests can assert call count/args.
type fakeTokenChecker struct {
	allowed bool
	err     error
	calls   []fakeCheckCall
}

func (f *fakeTokenChecker) CheckUseToken(_ context.Context, ns, name, credID, presented string, fullyConsistent bool) (bool, error) {
	f.calls = append(f.calls, fakeCheckCall{ns, name, credID, presented, fullyConsistent})
	return f.allowed, f.err
}

func checkTokenUseScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

const (
	checkTokenTestNS      = "ns"
	checkTokenTestSession = "sess-1"
	checkTokenTestEnvVar  = "TOKEN"
	checkTokenTestValue   = "presented-value"
	checkTokenTestHMACKey = "hmac-key-bytes"
)

// credDescriptor is the single credential every checkTokenUse test presents.
func credDescriptor() spiceboxv1alpha1.CredentialDescriptor {
	return spiceboxv1alpha1.CredentialDescriptor{
		Source: spiceboxv1alpha1.CredentialSource{
			Type: "static", Namespace: checkTokenTestNS, Name: "cred-secret", Key: "token",
		},
		Inject: spiceboxv1alpha1.CredentialInjection{EnvVar: checkTokenTestEnvVar},
	}
}

// newCheckTokenUseFixture builds a ToolCall (with one credential), the
// resolvedCall pointing at a bundle SpiceboxSession labeled with the parent
// AgentSession's name, the per-session args-hash-key Secret, and (unless
// omitAgentSession) the parent AgentSession itself — all in a fake client.
func newCheckTokenUseFixture(t *testing.T, omitAgentSession, omitSecret, omitLabel bool) (*Reconciler, *spiceboxv1alpha1.ToolCall, *resolvedCall, map[string]string, client.Client) {
	t.Helper()
	sch := checkTokenUseScheme(t)

	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-1", Namespace: checkTokenTestNS},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session:     "sbs-1",
			Tool:        "some-tool",
			Credentials: []spiceboxv1alpha1.CredentialDescriptor{credDescriptor()},
		},
	}

	sessLabels := map[string]string{agentSessionLabel: checkTokenTestSession}
	if omitLabel {
		sessLabels = nil
	}
	bundleSession := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sbs-1", Namespace: checkTokenTestNS, Labels: sessLabels},
	}
	resolved := &resolvedCall{session: bundleSession}

	objs := []client.Object{tc}
	if !omitSecret {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      checkTokenTestSession + spiceboxv1alpha1.MemoryTokenSecretSuffix,
				Namespace: checkTokenTestNS,
			},
			Data: map[string][]byte{argsHashKeySecretDataKey: []byte(checkTokenTestHMACKey)},
		})
	}
	if !omitAgentSession {
		objs = append(objs, &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: checkTokenTestSession, Namespace: checkTokenTestNS},
			Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
		})
	}

	c := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&spiceboxv1alpha1.ToolCall{}, &spiceboxv1alpha1.AgentSession{}).
		WithObjects(objs...).
		Build()

	r := &Reconciler{Client: c}
	agentEnv := map[string]string{checkTokenTestEnvVar: checkTokenTestValue}
	return r, tc, resolved, agentEnv, c
}

func getToolCall(t *testing.T, c client.Client, tc *spiceboxv1alpha1.ToolCall) spiceboxv1alpha1.ToolCall {
	t.Helper()
	var got spiceboxv1alpha1.ToolCall
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(tc), &got))
	return got
}

func getAgentSession(t *testing.T, c client.Client, ns, name string) spiceboxv1alpha1.AgentSession {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &got))
	return got
}

func TestCheckTokenUse_NoCredentialsOrNilChecker_Proceeds(t *testing.T) {
	cases := []struct {
		name       string
		clearCreds bool
		nilChecker bool
	}{
		{name: "empty tc.Spec.Credentials: proceed without calling the checker", clearCreds: true},
		{name: "nil TokenChecker (SpiceDB unconfigured): proceed without a checker call", nilChecker: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, toolCall, resolved, agentEnv, _ := newCheckTokenUseFixture(t, false, false, false)
			if tc.clearCreds {
				toolCall.Spec.Credentials = nil
			}
			checker := &fakeTokenChecker{allowed: false} // would deny if ever called
			if !tc.nilChecker {
				r.TokenChecker = checker
			}

			proceed, res, err := r.checkTokenUse(context.Background(), toolCall, resolved, agentEnv)
			require.NoError(t, err)
			assert.True(t, proceed, "must proceed: nothing to check")
			assert.Equal(t, ctrl.Result{}, res)
			assert.Empty(t, checker.calls, "checker must not be invoked")
		})
	}
}

func TestCheckTokenUse_Allow_ProceedsAndTouchesNothing(t *testing.T) {
	r, tc, resolved, agentEnv, c := newCheckTokenUseFixture(t, false, false, false)
	checker := &fakeTokenChecker{allowed: true}
	r.TokenChecker = checker

	proceed, res, err := r.checkTokenUse(context.Background(), tc, resolved, agentEnv)
	require.NoError(t, err)
	assert.True(t, proceed, "allow must let the caller continue to exec")
	assert.Equal(t, ctrl.Result{}, res)

	require.Len(t, checker.calls, 1)
	call := checker.calls[0]
	assert.Equal(t, checkTokenTestNS, call.ns)
	assert.Equal(t, checkTokenTestSession, call.name)
	assert.Equal(t, externaltoken.CredID(credDescriptor().Source), call.credID)
	assert.Equal(t, externaltoken.ValueHash([]byte(checkTokenTestHMACKey), checkTokenTestValue), call.presented,
		"presented hash must be computed over exactly the resolved env value, keyed by the session's args-hash key")
	assert.True(t, call.fullyConsistent, "use_token checks must always be fully-consistent, never best-effort")

	gotTC := getToolCall(t, c, tc)
	assert.False(t, hasTrueCondition(&gotTC, spiceboxv1alpha1.ToolCallConditionFailed), "ToolCall must not be failed on allow")

	gotSess := getAgentSession(t, c, checkTokenTestNS, checkTokenTestSession)
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, gotSess.Status.Phase, "AgentSession must not be failed on allow")
}

func TestCheckTokenUse_DefinitiveDeny_FailsToolCallOnly_SessionContinues(t *testing.T) {
	r, tc, resolved, agentEnv, c := newCheckTokenUseFixture(t, false, false, false)
	r.TokenChecker = &fakeTokenChecker{allowed: false, err: nil}

	proceed, _, err := r.checkTokenUse(context.Background(), tc, resolved, agentEnv)
	require.NoError(t, err, "a definitive deny is not a Reconcile error")
	assert.False(t, proceed, "caller must stop before exec")

	gotTC := getToolCall(t, c, tc)
	require.True(t, hasTrueCondition(&gotTC, spiceboxv1alpha1.ToolCallConditionFailed), "ToolCall must be Failed")
	cond := findCondition(gotTC.Status.Conditions, spiceboxv1alpha1.ToolCallConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentCredentialRevoked, cond.Reason)
	require.NotNil(t, gotTC.Status.FinishedAt)

	gotSess := getAgentSession(t, c, checkTokenTestNS, checkTokenTestSession)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, gotSess.Status.Phase,
		"a definitive deny is surgical: the session must NOT be failed")
}

func TestCheckTokenUse_Indeterminate_FailsBothToolCallAndSession(t *testing.T) {
	cases := []struct {
		name       string
		omitSecret bool
		checkerErr error
	}{
		{name: "CheckUseToken RPC error", checkerErr: errors.New("spicedb unreachable")},
		{name: "args-hash-key Secret missing", omitSecret: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, toolCall, resolved, agentEnv, c := newCheckTokenUseFixture(t, false, tc.omitSecret, false)
			checker := &fakeTokenChecker{allowed: true, err: tc.checkerErr}
			r.TokenChecker = checker

			proceed, _, err := r.checkTokenUse(context.Background(), toolCall, resolved, agentEnv)
			require.NoError(t, err, "the AgentSession-fail write itself succeeds against the fake client")
			assert.False(t, proceed)

			gotSess := getAgentSession(t, c, checkTokenTestNS, checkTokenTestSession)
			assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, gotSess.Status.Phase,
				"indeterminate must fail-closed the parent AgentSession")
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionTokenAuthzUnavailable, gotSess.Status.FailureReason)
			sessCond := findCondition(gotSess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
			require.NotNil(t, sessCond)
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionTokenAuthzUnavailable, sessCond.Reason)

			gotTC := getToolCall(t, c, toolCall)
			require.True(t, hasTrueCondition(&gotTC, spiceboxv1alpha1.ToolCallConditionFailed), "ToolCall must also be Failed so exec does not run")
			tcCond := findCondition(gotTC.Status.Conditions, spiceboxv1alpha1.ToolCallConditionFailed)
			require.NotNil(t, tcCond)
			assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionTokenAuthzUnavailable, tcCond.Reason)
		})
	}
}

func TestCheckTokenUse_MissingAgentSessionLabel_FailsToolCallOnly(t *testing.T) {
	r, tc, resolved, agentEnv, c := newCheckTokenUseFixture(t, true /* no AgentSession object either — name is unknown */, false, true)
	checker := &fakeTokenChecker{allowed: true}
	r.TokenChecker = checker

	proceed, _, err := r.checkTokenUse(context.Background(), tc, resolved, agentEnv)
	require.NoError(t, err)
	assert.False(t, proceed)
	assert.Empty(t, checker.calls, "no session identity to check against — the checker must not be called")

	gotTC := getToolCall(t, c, tc)
	require.True(t, hasTrueCondition(&gotTC, spiceboxv1alpha1.ToolCallConditionFailed))
	cond := findCondition(gotTC.Status.Conditions, spiceboxv1alpha1.ToolCallConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionTokenAuthzUnavailable, cond.Reason)
}

func TestCheckTokenUse_HeaderOnlyDescriptor_SkippedNotChecked(t *testing.T) {
	r, tc, resolved, agentEnv, _ := newCheckTokenUseFixture(t, false, false, false)
	// Header-injected descriptors (MCP's shape) carry no EnvVar; the sandbox
	// check has nothing to present for them and must skip, not crash or deny.
	tc.Spec.Credentials = []spiceboxv1alpha1.CredentialDescriptor{{
		Source: spiceboxv1alpha1.CredentialSource{Type: "static", Namespace: checkTokenTestNS, Name: "cred-secret", Key: "token"},
		Inject: spiceboxv1alpha1.CredentialInjection{Header: &spiceboxv1alpha1.HeaderInjection{Name: "Authorization"}},
	}}
	checker := &fakeTokenChecker{allowed: false} // would deny if ever (wrongly) called
	r.TokenChecker = checker

	proceed, _, err := r.checkTokenUse(context.Background(), tc, resolved, agentEnv)
	require.NoError(t, err)
	assert.True(t, proceed)
	assert.Empty(t, checker.calls)
}

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
