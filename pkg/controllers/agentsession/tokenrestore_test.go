package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
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

func TestRestoreTokenRegistry(t *testing.T) {
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

	n, err := r.RestoreTokenRegistry(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, n, "every session not being deleted is looked at")

	key := func(name string) memory.NamespacedName { return memory.NamespacedName{Namespace: "default", Name: name} }
	got, ok := reg.Lookup("live-tok")
	assert.True(t, ok, "a live session's token is back")
	assert.Equal(t, key("live"), got)
	assert.True(t, reg.Authorizes("live-tok", key("live-gitlike")), "with the bundle scopes it had before")
	assert.False(t, reg.Registered(key("deleting")), "a deleting session is skipped: its token may already be revoked")
	assert.False(t, reg.Registered(key("no-secret")), "a session with no Secret gets nothing; restore never mints")
	assert.True(t, reg.Restoring(), "ending the restoring state is the runnable's job, after the pass")
}

func TestTokenRestoreRunnable_FinishesRestore(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	live := restoreSession("live", false)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, tokenSecret(live, "live-tok")).Build()

	reg := tokens.NewRegistry()
	reg.BeginRestore()
	run := &tokenRestoreRunnable{r: &Reconciler{Client: c, Tokens: reg}, cache: syncedCache{}}
	assert.False(t, run.NeedLeaderElection(), "every replica serves the memory API, so every replica restores")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, run.Start(ctx))
	assert.False(t, reg.Restoring(), "the restoring state ends once the pass is done")
	_, ok := reg.Lookup("live-tok")
	assert.True(t, ok, "and the token is in place before it ends")
}

type syncedCache struct{}

func (syncedCache) WaitForCacheSync(context.Context) bool { return true }
