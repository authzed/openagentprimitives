// pkg/controllers/credentialupdaterequest/agentowned_fulfill_test.go
//
// The loop-closing proof for the agent-owned credential-update write path.
//
// identityd's own tests assert that an authorized admin's click moves the
// AgentIdentity's backing Secret. That is only half the claim worth making: the
// request is marked Fulfilled by THIS reconciler's secretChangedSinceOpen,
// which compares a content hash of exactly ONE Secret -- the one recorded on
// status.credentialSecretRef when the card opened. A write that lands on a
// different Secret (the admin's own UserIdentity master Secret, say) satisfies
// "something was written" while leaving the request to run out its window and
// then announce that nobody updated the credential.
//
// So the test below writes through the REAL production path
// (agentidentity.PutToken, the same call identityd's handler makes) rather than
// hand-editing a Secret, and asserts the reconciler reaches Fulfilled. If the
// write path is ever pointed at another object, this goes red -- the assertion
// is about the two halves agreeing, not about either one in isolation.
package credentialupdaterequest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/credentialupdaterequest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/agentidentity"
)

const (
	agentIDName     = "demo-agent-identity"
	agentSecretName = "demo-agent-secret"
)

// openAgentOwnedRequest drives an AgentIdentity-backed request to Open and
// returns the client + reconciler, mirroring unpark_test.go's
// openStaticRequest for the agent-owned shape.
func openAgentOwnedRequest(t *testing.T) (client.Client, *credentialupdaterequest.Reconciler) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // definitive rejection -> Open
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: sessionName, Namespace: ns, UID: sessionUID},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: className, AgentIdentity: agentIDName},
	}
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: className, Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: spiceboxv1alpha1.IdentityModeAgent},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: agentIDName, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: credName, Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: agentSecretName, Key: credName},
				},
			}},
		},
	}
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: mcpName, Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: credName, Provider: "github-pat"},
		},
	}
	cur := &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: curName, Namespace: ns,
			OwnerReferences:   []metav1.OwnerReference{ownerRef()},
			CreationTimestamp: metav1.Now(),
		},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef:  spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: sessionName},
			Origin:      "mcpserver/" + mcpName,
			ToolName:    "demo_tool",
			RequestedBy: identity.Subject("user:demo-user"),
		},
	}
	agentSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: agentSecretName, Namespace: ns},
		Data:       map[string][]byte{credName: []byte("tok-dead")},
	}

	c, r, _ := newReconciler(t, sess, class, ai, mcp, cur, agentSecret)
	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "first Reconcile must open the request")

	got := getCUR(t, c)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase, "status=%+v", got.Status)
	require.NotNil(t, got.Status.CredentialSecretRef, "Open must record the Secret this reconciler will watch")
	require.NotEmpty(t, got.Status.CredentialSecretObservedHash, "Open must record the baseline hash")
	return c, r
}

// TestAgentOwnedWriteDrivesTheRequestToFulfilled is the invariant that closes
// the loop: the production write path moves the exact Secret this reconciler
// watches, so the request reaches Fulfilled instead of expiring with
// "nobody updated the credential".
func TestAgentOwnedWriteDrivesTheRequestToFulfilled(t *testing.T) {
	c, r := openAgentOwnedRequest(t)
	before := getCUR(t, c)

	// The SAME call identityd's handler makes, with the SAME expected-Secret
	// guard it passes -- so the destination is proven, not assumed.
	target, err := agentidentity.PutToken(context.Background(), c, agentidentity.PutTokenRequest{
		Namespace: ns, Name: agentIDName, CredentialName: credName,
		Token:        "tok-fresh",
		ExpectSecret: before.Status.CredentialSecretRef,
	})
	require.NoError(t, err, "the admin's write must be accepted")
	assert.Equal(t, before.Status.CredentialSecretRef.Namespace, target.SecretRef.Namespace)
	assert.Equal(t, before.Status.CredentialSecretRef.Name, target.SecretRef.Name,
		"the write must land on the very Secret the request recorded")

	_, err = reconcileOnce(t, r)
	require.NoError(t, err, "second Reconcile")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, got.Status.Phase,
		"the recorded Secret changed, so the request must Fulfil; status=%+v", got.Status)
	assert.Equal(t, before.Status.Determination, got.Status.Determination,
		"Fulfilling must preserve the ORIGINAL determination that justified the card")
}

// TestAgentOwnedWriteToTheWrongSecretDoesNotFulfil is the negative control that
// makes the test above mean something. Writing a fresh value into a DIFFERENT
// Secret in the same namespace -- the shape of the bug this slice removes, where
// the pasted value went to the clicker's own identity -- leaves the request
// stuck Open, on its way to the false "nobody updated the credential" message.
func TestAgentOwnedWriteToTheWrongSecretDoesNotFulfil(t *testing.T) {
	c, r := openAgentOwnedRequest(t)

	decoy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "somebody-elses-secret", Namespace: ns},
		Data:       map[string][]byte{credName: []byte("tok-fresh")},
	}
	require.NoError(t, c.Create(context.Background(), decoy))

	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "second Reconcile")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase,
		"a write anywhere but the recorded Secret must NOT be mistaken for a fix")
}
