// pkg/controllers/agentidentity/credential_shape_test.go
//
// A credential of the wrong SHAPE used to sail through every readiness gate.
// The AgentIdentity went Valid=True, the AgentClass Valid=True, the session
// Running — and the mistake only surfaced minutes later, inside a tool's own
// output, as an opaque 401 from the upstream provider.
//
// The provider catalog already declares each credential's expected format
// (provider.TokenShape) and every INTERACTIVE entry point already enforces it —
// the identityd paste forms, the put-token CLI, the builtin setup flows. A
// Secret created out of band (`kubectl create secret generic ...`) passes
// through none of them. This reconciler is where such a Secret is adopted, so
// it is where the same declared format has to be checked.
package agentidentity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"

	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// mcpServerFor declares that credName is backed by the given provider-catalog
// id — one of the two routes passthroughcatalog.ProviderForCredential resolves
// (the other being a toolkit env binding, exercised separately below).
func mcpServerFor(credName, providerID string) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "demo-mcp"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{
				URL:       "https://mcp.demo.test/rpc",
				Transport: "http",
			},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Type:       passthroughcatalog.AuthTypeStatic,
				Provider:   providerID,
				Credential: credName,
			},
		},
	}
}

// reconcileShape builds an AgentIdentity with one type=static credential whose
// Secret holds value, runs one Reconcile, and returns the reloaded object.
func reconcileShape(t *testing.T, credName, value string, extra ...client.Object) *spiceboxv1alpha1.AgentIdentity {
	t.Helper()
	const aiName = "ai-shape"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "demo-shape-secret"},
		Data:       map[string][]byte{"token": []byte(value)},
	}
	adoptguard.WithAdoptedLabel(sec)

	ai := aiWithCreds(testNS, aiName, spiceboxv1alpha1.AgentCredential{
		Name: credName, Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demo-shape-secret", Key: "token"},
		},
	})

	objs := append([]client.Object{sec, ai}, extra...)
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).
		Build()
	r := &Reconciler{
		Client:       c,
		APIReader:    c,
		SecretReader: adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
	}
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: testNS, Name: aiName},
	})
	require.NoError(t, err, "a shape mismatch is a config problem, reported on the condition — never a returned (retried) error")

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: aiName}, &got))
	return &got
}

// TestReconcile_ChecksTheDeclaredCredentialShape covers every way the declared
// format can land: it matches, it does not, the provider declares none, and the
// credential resolves to no provider at all. The last two must stay permissive
// — never refuse a credential whose expected format we do not know.
func TestReconcile_ChecksTheDeclaredCredentialShape(t *testing.T) {
	cases := []struct {
		name       string
		credName   string
		value      string
		extra      []client.Object
		wantStatus metav1.ConditionStatus
		wantReason string
		// wantMsgContains are substrings the condition message must carry.
		wantMsgContains []string
	}{
		{
			name:       "value matches the provider's declared shape: Valid=True",
			credName:   "demo-api-cred",
			value:      "sk-ant-api03-demo-value",
			extra:      []client.Object{mcpServerFor("demo-api-cred", "anthropic-api-key")},
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonAllReferencesResolve,
		},
		{
			name:       "wrong-shape value: Valid=False/CredentialShapeMismatch naming the expected shape AND the one it actually looks like",
			credName:   "demo-api-cred",
			value:      "sk-ant-oat01-demo-value",
			extra:      []client.Object{mcpServerFor("demo-api-cred", "anthropic-api-key")},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonCredentialShapeMismatch,
			wantMsgContains: []string{
				"demo-api-cred",   // which credential
				"sk-ant-api",      // the shape this credential expects
				"anthropic-oauth", // the provider whose shape it actually matches
				"sk-ant-oat",      // and that shape, so the fix is obvious
			},
		},
		{
			name:       "provider declares no token shape: Valid=True (never refuse a format we do not know)",
			credName:   "demo-oauth-cred",
			value:      "anything-at-all",
			extra:      []client.Object{mcpServerFor("demo-oauth-cred", "oauth-mcp")},
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonAllReferencesResolve,
		},
		{
			name:       "credential backed by no provider: Valid=True (nothing declares a shape to check)",
			credName:   "demo-unbacked-cred",
			value:      "anything-at-all",
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonAllReferencesResolve,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileShape(t, tc.credName, tc.value, tc.extra...)
			cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
			require.NotNil(t, cond, "Valid condition must be present")
			assert.Equal(t, tc.wantStatus, cond.Status)
			assert.Equal(t, tc.wantReason, cond.Reason)
			for _, want := range tc.wantMsgContains {
				assert.Contains(t, cond.Message, want)
			}
			if tc.wantStatus == metav1.ConditionFalse {
				assert.Empty(t, got.Status.ResolvedCredentials,
					"a credential of the wrong shape must not be reported as resolved")
				assert.NotContains(t, cond.Message, tc.value,
					"the message must never echo the credential value back")
			}
		})
	}
}

// TestReconcile_ShapeComesFromTheToolkitEnvBinding pins the OTHER resolution
// route, and the one behind the live incident: a toolkit's env binding names
// both the env var and the provider backing it, so a credential filling
// ANTHROPIC_API_KEY is checked against the API-key shape even with no MCPServer
// in the cluster. The subscription OAuth token that belongs in the sibling
// toolkit's CLAUDE_CODE_OAUTH_TOKEN is the predictable mistake here.
func TestReconcile_ShapeComesFromTheToolkitEnvBinding(t *testing.T) {
	// Derived, not asserted blind: the binding this test depends on is read back
	// out of the embedded catalog, so a toolkit that renames the credential
	// fails HERE with a message saying so rather than silently losing coverage.
	const credName = "anthropic-api-key"
	require.Equal(t, "anthropic-api-key", passthroughcatalog.ProviderIDFromToolkits(credName),
		"a toolkit env binding must map this credential name to the API-key provider; if the toolkit catalog renamed either side, update this fixture")

	// The subscription OAuth token from `claude setup-token`, pasted into the
	// credential that fills ANTHROPIC_API_KEY. Both are sk-ant- prefixed and
	// neither works in the other's place.
	got := reconcileShape(t, credName, "sk-ant-oat01-demo-value")

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
	require.NotNil(t, cond, "Valid condition must be present")
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"an unusable credential must not report Valid=True and defer the failure to the first turn")
	assert.Equal(t, spiceboxv1alpha1.ReasonCredentialShapeMismatch, cond.Reason)
	assert.Contains(t, cond.Message, "sk-ant-api", "the message must name the shape this credential expects")
	assert.Contains(t, cond.Message, "sk-ant-oat", "and the shape the supplied value actually has")
	assert.Empty(t, got.Status.ResolvedCredentials)
}
