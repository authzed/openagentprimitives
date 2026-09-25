//go:build integration

// pkg/controllers/agentsession/runner_identity_rbac_e2e_test.go
package agentsession_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// TestRunnerSAReadsPinnedAgentIdentityViaRBAC is the end-to-end authorization
// check for the toolBundle credential-resolution fix. The unit test
// (TestReconcileToolBundlePinsAgentIdentity) asserts the per-session Role
// *contains* the agentidentities rule; this test goes one step further and
// asserts the envtest apiserver actually GRANTS the runner ServiceAccount `get`
// on the pinned AgentIdentity through the real Role→RoleBinding→SA chain — the
// exact authorization the runner pod relies on when it reads the identity to
// resolve a sandbox-tool credential (e.g. GITHUB_TOKEN for gh). This is the
// real-world failure the user hit: "agentidentities ... is forbidden: User
// system:serviceaccount:...-runner-sa cannot get resource agentidentities".
//
// The in-process E2E harness can't cover this: its runner uses the manager's
// (admin) client, so it never exercises the per-session SA's RBAC. Impersonating
// the SA against the real apiserver is the faithful end-to-end check.
//
// Self-validating: the negative control (a different, non-pinned AgentIdentity
// must be FORBIDDEN) proves the apiserver enforces RBAC. Without enforcement the
// negative case would also succeed and fail this assertion — so a green test
// means the positive case is meaningful, not vacuous.
func TestRunnerSAReadsPinnedAgentIdentityViaRBAC(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-rbac-class"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi")},
			Tools:     []spiceboxv1alpha1.SpiceboxTool{{Name: "gh", Command: []string{"/usr/bin/gh"}}},
		},
	}), "create SpiceboxClass")

	// The identity the runner SA must be able to read...
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "pinned-id", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "github-token", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "gh-token-secret", Key: "token"},
				},
			}},
		},
	}), "create pinned AgentIdentity")
	// ...and a second the runner SA must NOT be able to read (negative control).
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "other-id", Namespace: "default"},
	}), "create non-pinned AgentIdentity")

	ac := validClass("ac-gh-rbac")
	ac.Spec.AgentIdentity = "pinned-id"
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
		{Name: "github", Class: "gh-rbac-class", Toolspecs: []string{}},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("ghrbac", "ac-gh-rbac")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	for i := 0; i < 5; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}
	// The runner SA/Role/RoleBinding are built once the bundle is Ready.
	var bs spiceboxv1alpha1.SpiceboxSession
	require.NoError(t,
		env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ghrbac-github"}, &bs),
		"bundle SpiceboxSession should be created")
	bs.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.SpiceboxSessionConditionReady, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonPodReady, LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, &bs), "stamp bundle Ready=True")
	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)})
	}

	// Sanity: the SA exists (one leg of the chain the apiserver evaluates).
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ghrbac-runner-sa"}, &corev1.ServiceAccount{}),
		"runner ServiceAccount should exist")

	// Build a client that impersonates the runner ServiceAccount — exactly how
	// the runner pod's in-cluster credentials authenticate to the apiserver.
	saCfg := rest.CopyConfig(env.Cfg)
	saCfg.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:default:ghrbac-runner-sa",
	}
	saClient, err := client.New(saCfg, client.Options{Scheme: env.Scheme})
	require.NoError(t, err, "build runner-SA-impersonating client")

	// Positive: the runner SA CAN read its pinned AgentIdentity (the fix).
	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t,
		saClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "pinned-id"}, &got),
		"runner SA must be allowed to get its pinned AgentIdentity — this is what the fix grants")

	// Negative control: the runner SA is FORBIDDEN to read a different identity.
	// Confirms the rule is pinned by name AND that the apiserver enforces RBAC
	// (so the positive assertion above is meaningful, not vacuous).
	err = saClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "other-id"}, &spiceboxv1alpha1.AgentIdentity{})
	assert.True(t, apierrors.IsForbidden(err),
		"runner SA must be FORBIDDEN to get a non-pinned AgentIdentity; got: %v", err)

	// The exiting runner stamps the wake-requested-at annotation on its own
	// session when a message lands as it idles. That is a metadata patch, not
	// the status subresource the runner already owns — the in-process E2E
	// harness runs the runner on the manager's admin client and would never
	// notice the missing grant, so assert the real Role→RoleBinding→SA chain
	// authorizes it against an enforcing apiserver.
	t.Run("runner SA can stamp its own wake annotation, and only its own", func(t *testing.T) {
		var own spiceboxv1alpha1.AgentSession
		require.NoError(t, saClient.Get(ctx, client.ObjectKeyFromObject(sess), &own), "runner SA gets its own session")
		base := own.DeepCopy()
		if own.Annotations == nil {
			own.Annotations = map[string]string{}
		}
		own.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = time.Now().UTC().Format(time.RFC3339Nano)
		require.NoError(t, saClient.Patch(ctx, &own, client.MergeFrom(base)),
			"runner SA must be allowed to patch its OWN AgentSession — without it a post-idle wake 403s and the message strands")

		// Negative control: a different session in the same namespace is off
		// limits, proving the grant is pinned and the apiserver enforces it.
		other := validSession("ghrbac-other", "ac-gh-rbac")
		require.NoError(t, env.Client.Create(ctx, other), "create second AgentSession")
		otherBase := other.DeepCopy()
		other.Annotations = map[string]string{spiceboxv1alpha1.AnnotationWakeRequestedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		perr := saClient.Patch(ctx, other, client.MergeFrom(otherBase))
		assert.True(t, apierrors.IsForbidden(perr),
			"runner SA must be FORBIDDEN to patch another session; got: %v", perr)
	})
}
