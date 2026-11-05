package agentsession

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

func restoreSession(name string, deleting bool) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			BundleSessions: []spiceboxv1alpha1.ResolvedBundle{
				{Name: "gitlike", SpiceboxSessionName: name + "-gitlike"},
			},
		},
	}
	if deleting {
		now := metav1.Now()
		s.DeletionTimestamp = &now
		s.Finalizers = []string{spiceboxv1alpha1.FinalizerAgentSession}
	}
	return s
}

func tokenSecret(sess *spiceboxv1alpha1.AgentSession, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: sess.Namespace, Name: MemoryTokenSecretName(sess)},
		Data:       map[string][]byte{agentSessionSecretMemoryToken: []byte(token)},
	}
}

func TestRestoreTokens(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	live := restoreSession("live", false)
	deleting := restoreSession("deleting", true)
	noSecret := restoreSession("no-secret", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		live, deleting, noSecret,
		tokenSecret(live, "live-tok"),
		tokenSecret(deleting, "deleting-tok"),
	).Build()

	reg := tokens.NewRegistry()
	reg.BeginRestore()
	r := &Reconciler{Client: c, Tokens: reg}

	n, err := r.RestoreTokens(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "counts the tokens registered, not the sessions looked at")

	key := func(name string) memory.NamespacedName {
		return memory.NamespacedName{Namespace: "default", Name: name}
	}
	got, ok := reg.Lookup("live-tok")
	assert.True(t, ok, "a live session's token is back")
	assert.Equal(t, key("live"), got)
	assert.True(t, reg.Authorizes("live-tok", key("live-gitlike")), "with the bundle scopes it had before")
	assert.False(t, reg.Registered(key("deleting")), "a deleting session is skipped: its token may already be revoked")
	assert.False(t, reg.Registered(key("no-secret")), "a session with no Secret gets nothing; restore never mints")
	assert.True(t, reg.Restoring(), "ending the restoring state is the runnable's job, after the pass")
}

func TestRestoreTokens_SecretReadFailureIsReturned(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	broken := restoreSession("broken", false)
	fine := restoreSession("fine", false)
	brokenSecret := MemoryTokenSecretName(broken)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		broken, fine,
		tokenSecret(broken, "broken-tok"),
		tokenSecret(fine, "fine-tok"),
	).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == brokenSecret {
				return errors.New("apiserver hiccup")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()

	reg := tokens.NewRegistry()
	r := &Reconciler{Client: c, Tokens: reg}

	n, err := r.RestoreTokens(context.Background())
	assert.ErrorContains(t, err, "apiserver hiccup", "a failed read is returned so the pass is retried")
	assert.Equal(t, 1, n, "and does not stop the other session's restore")
	key := func(name string) memory.NamespacedName {
		return memory.NamespacedName{Namespace: "default", Name: name}
	}
	assert.True(t, reg.Registered(key("fine")))
	assert.False(t, reg.Registered(key("broken")))
}

// TestRestoreTokens_GuardPanicSkipsThatSession pins that the guarded reader's
// panic on an unadopted Secret skips one session instead of crashing the
// process. The pass runs on errgroup goroutines outside any Reconcile, so
// nothing else would recover it, and the operator would crash on every boot.
// It is not returned as an error either: it would recur on every retry and
// hold every Reconcile behind WaitRestored.
func TestRestoreTokens_GuardPanicSkipsThatSession(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	unadopted := restoreSession("unadopted", false)
	adopted := restoreSession("adopted", false)
	adoptedSecret := tokenSecret(adopted, "adopted-tok")
	adoptedSecret.Labels = map[string]string{adoptguard.AdoptedLabel: "true"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		unadopted, adopted,
		tokenSecret(unadopted, "unadopted-tok"), // no adopted label: the guard panics on it
		adoptedSecret,
	).Build()

	reg := tokens.NewRegistry()
	r := &Reconciler{
		Client:       c,
		Tokens:       reg,
		SecretReader: adoptguard.NewSecretReader(c, c, adoptguard.Panic, func(types.NamespacedName) bool { return false }),
	}

	var (
		n   int
		err error
	)
	require.NotPanics(t, func() { n, err = r.RestoreTokens(context.Background()) })
	assert.NoError(t, err, "a panic is logged and skipped, not retried")
	assert.Equal(t, 1, n, "the other session is still restored")
	assert.True(t, reg.Registered(memory.NamespacedName{Namespace: "default", Name: "adopted"}))
	assert.False(t, reg.Registered(memory.NamespacedName{Namespace: "default", Name: "unadopted"}),
		"the skipped session is left to its own Reconcile")
}

func TestRestoreTokens_ForbiddenReadIsSkippedNotRetried(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	forbidden := restoreSession("forbidden", false)
	fine := restoreSession("fine", false)
	forbiddenSecret := MemoryTokenSecretName(forbidden)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		forbidden, fine,
		tokenSecret(forbidden, "forbidden-tok"),
		tokenSecret(fine, "fine-tok"),
	).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == forbiddenSecret {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, errors.New("no RBAC"))
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()

	reg := tokens.NewRegistry()
	r := &Reconciler{Client: c, Tokens: reg}

	n, err := r.RestoreTokens(context.Background())
	assert.NoError(t, err, "Forbidden will not clear on retry, so it must not hold the registry restoring")
	assert.Equal(t, 1, n)
	key := func(name string) memory.NamespacedName {
		return memory.NamespacedName{Namespace: "default", Name: name}
	}
	assert.True(t, reg.Registered(key("fine")))
	assert.False(t, reg.Registered(key("forbidden")), "left to the session's own Reconcile")
}
