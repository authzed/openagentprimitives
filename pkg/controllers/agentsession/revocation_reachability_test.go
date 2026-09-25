// pkg/controllers/agentsession/revocation_reachability_test.go
//
// Reconcile-level (fake-client) tests that pin REACHABILITY of the
// credential-grant diff: revoking a credential must delete its
// authorized_token grant no matter what state the session is parked in.
//
// reconcileCredentialGrants' declarative diff is the ONLY thing that deletes a
// revoked credential's grant (see the long placement comment at its call site
// in controller.go). Its own doc states it "MUST run unconditionally for every
// phase that reaches this line" — but "reaches this line" is the loophole these
// tests close: several earlier returns park the reconcile before it, and
// nothing re-enqueues the session afterward, so the revoked grant survives
// INDEFINITELY rather than late.
//
// Both cases below drive the full Reconcile with a granter holding one stale
// grant and a session surface that entitles it to none (no AgentIdentity, no
// MCPServers/bundles/sidecars), so the correct diff is exactly one delete.
package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
)

// revokedCredID is the grant the granter starts holding and that every case
// below expects deleted — it models a credential just removed from the
// AgentIdentity.
const revokedCredID = "cred-just-revoked"

// revocationClass returns an AgentClass with no credential surface at all (no
// AgentIdentity, MCPServers, ToolBundles or SidecarToolboxes), so the desired
// grant set reconcileCredentialGrants computes is empty and every grant the
// granter currently holds is stale. valid controls the Valid condition.
func revocationClass(name string, valid bool) *spiceboxv1alpha1.AgentClass {
	status := metav1.ConditionFalse
	reason := spiceboxv1alpha1.ReasonAgentIdentityInvalid
	if valid {
		status = metav1.ConditionTrue
		reason = spiceboxv1alpha1.ReasonAllReferencesResolve
	}
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode: spiceboxv1alpha1.IdentityModeAgent,
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic", Name: "claude-opus-4-7",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 50, MaxTokens: 100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
		},
		Status: spiceboxv1alpha1.AgentClassStatus{Conditions: []metav1.Condition{{
			Type: spiceboxv1alpha1.AgentClassConditionValid, Status: status,
			Reason: reason, LastTransitionTime: metav1.Now(),
		}}},
	}
}

// revocationSession returns a live (already-booted) session in the given phase.
// The finalizer is pre-set so the reconcile does not short-circuit on
// EnsureFinalizer, and channelAttached decides whether the idle-sleep/archive
// sweep applies to it.
func revocationSession(name, class, phase string, channelAttached bool) *spiceboxv1alpha1.AgentSession {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID("uid-" + name),
			Finalizers: []string{spiceboxv1alpha1.FinalizerAgentSession},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  class,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hello"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
	if channelAttached {
		sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-ch", Kind: "slack", Key: "dm:U123",
			Capabilities:      []string{"text"},
			NATSSubjectPrefix: "ap.session.default." + name,
		}
	}
	return sess
}

// perSessionSecret returns the per-session Secret carrying the args-hash key
// the grant diff binds static credential values under. Its presence marks the
// session as one that has already booted (and therefore already had grants
// written for it).
func perSessionSecret(sess *spiceboxv1alpha1.AgentSession) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      MemoryTokenSecretName(sess),
			Namespace: sess.Namespace,
		},
		Data: map[string][]byte{
			"token":                       []byte("session-token"),
			agentSessionSecretArgsHashKey: []byte("0123456789abcdef0123456789abcdef"),
		},
	}
}

// newRevocationReconciler wires a Reconciler over a fake client holding objs,
// with a granter that already holds the revoked grant. It returns the granter
// so the test can assert the diff that was applied.
func newRevocationReconciler(t *testing.T, objs ...client.Object) (*Reconciler, *fakeTokenGranter) {
	t.Helper()
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme, networkingv1.AddToScheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.SpiceboxSession{}).
		Build()
	granter := &fakeTokenGranter{current: []externaltoken.AuthorizedTokenGrant{
		{CredID: revokedCredID, AuthorizedValueHash: "stale-hash"},
	}}
	r := &Reconciler{
		Client:       c,
		APIReader:    c,
		Tokens:       tokens.NewRegistry(),
		Memory:       memory.NewLocal(inmem.NewBackend()),
		TokenGranter: granter,
		Now:          func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) },
	}
	r.RunnerFactory = &PodRunnerFactory{
		Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222",
	}
	return r, granter
}

// TestRevocationReachesGrantDiffFromEveryParkedState asserts the invariant the
// grant-diff call site claims but does not currently hold: a revoke lands
// regardless of the state the session is parked in.
//
// Kept as sibling subtests rather than a table — the two cases park the
// reconcile via different mechanisms and share only their assertion.
func TestRevocationReachesGrantDiffFromEveryParkedState(t *testing.T) {
	ctx := context.Background()

	// An AgentClass flips Valid=False the moment a credential is removed from
	// the AgentIdentity it references (the agentclass controller reacts to the
	// identity going invalid). That is the SAME write that triggers the
	// session's own re-enqueue, so the two race — and when the class-invalid
	// view wins, the reconcile returns at the class gate and the revoked grant
	// is never diffed away. Nothing re-enqueues on a class becoming invalid, so
	// the grant then survives indefinitely.
	t.Run("invalid AgentClass: revoked grant is still deleted", func(t *testing.T) {
		class := revocationClass("cls-invalid", false)
		sess := revocationSession("s-invalid", class.Name, spiceboxv1alpha1.AgentSessionPhaseRunning, false)
		r, granter := newRevocationReconciler(t, class, sess, perSessionSecret(sess))

		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name},
		})
		require.NoError(t, err, "reconcile of a session whose class is invalid must not error")

		assert.Equal(t, []string{revokedCredID}, granter.deleted,
			"the revoked credential's grant must be deleted even though the AgentClass is not Valid")
	})

	// An Idle, channel-attached, not-woken session is handled entirely by the
	// idle-sleep/archive sweep, which returns before the grant diff. This is
	// the RESTING state of every chat agent, and the grant-diff call site's own
	// doc names Idle as a phase it covers.
	t.Run("idle channel session: revoked grant is still deleted", func(t *testing.T) {
		class := revocationClass("cls-idle", true)
		sess := revocationSession("s-idle", class.Name, spiceboxv1alpha1.AgentSessionPhaseIdle, true)
		r, granter := newRevocationReconciler(t, class, sess, perSessionSecret(sess))

		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name},
		})
		require.NoError(t, err, "reconcile of a parked idle session must not error")

		assert.Equal(t, []string{revokedCredID}, granter.deleted,
			"the revoked credential's grant must be deleted even though the session is parked Idle")
	})
}
