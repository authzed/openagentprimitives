package clusterskillsource

import (
	"context"
	"errors"
	"testing"
	"time"

	bundlemem "github.com/authzed/openagentprimitives/pkg/tools/skillbundle/memory"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/skillfetch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reqNames flattens mapper output for readable assertions. ClusterSkillSource is
// cluster-scoped, so a request carries a bare name.
func reqNames(reqs []reconcile.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Name)
	}
	return out
}

// TestMapSecretToSources covers the missing watch. The cluster source reads its
// Secret directly at spec.auth.namespace, so the match is namespace+name — and
// the namespace half matters: an unrelated "pat" Secret elsewhere in the cluster
// must not wake every ClusterSkillSource.
func TestMapSecretToSources(t *testing.T) {
	cases := []struct {
		name   string
		secret *corev1.Secret
		want   []string
	}{
		{
			name:   "secret backing the credential: enqueues its ClusterSkillSource",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pat", Namespace: "secrets-ns"}},
			want:   []string{"src"},
		},
		{
			name:   "same secret name in a different namespace: no requests",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pat", Namespace: "other-ns"}},
			want:   []string{},
		},
		{
			name:   "unrelated secret in the auth namespace: no requests",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "secrets-ns"}},
			want:   []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, sec := fixtures()
			c := newClient(t, src, sec)
			r := &Reconciler{Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New()}

			got := r.mapSecretToSources(context.Background(), tc.secret)
			assert.ElementsMatch(t, tc.want, reqNames(got))
		})
	}
}

// TestMapSecretToSourcesIgnoresAuthlessSource proves a public ClusterSkillSource
// is never woken by a Secret event.
func TestMapSecretToSourcesIgnoresAuthlessSource(t *testing.T) {
	src, sec := fixtures()
	src.Spec.Auth = nil
	c := newClient(t, src, sec)
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New()}

	got := r.mapSecretToSources(context.Background(), sec)
	assert.Empty(t, got)
}

// TestFailureRequeuesFasterThanSyncInterval mirrors the namespaced controller:
// a failing pass retries on the failure cadence, not the 1h success cadence.
func TestFailureRequeuesFasterThanSyncInterval(t *testing.T) {
	src, sec := fixtures()
	c := newClient(t, src, sec)
	r := &Reconciler{
		Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New(),
		SyncInterval: time.Hour,
		Fetcher:      &skillfetch.Fake{Err: errors.New("authentication required: Invalid username or token")},
	}

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: srcKey})
	require.NoError(t, err, "fetch failures surface as conditions, not returned errors")

	assert.Equal(t, failureRetryInterval, res.RequeueAfter)
	assert.Less(t, res.RequeueAfter, time.Hour)
}

// TestSuccessRequeuesOnSyncInterval guards that the short retry is failure-only.
func TestSuccessRequeuesOnSyncInterval(t *testing.T) {
	src, sec := fixtures()
	c := newClient(t, src, sec)
	r := &Reconciler{
		Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New(),
		SyncInterval: time.Hour,
		Fetcher:      &skillfetch.Fake{Result: skillfetch.Result{SHA: "abc123"}},
	}

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: srcKey})
	require.NoError(t, err)
	assert.Equal(t, time.Hour, res.RequeueAfter)
}

// compile-time guard: the mapper must satisfy handler.MapFunc's shape.
var _ func(context.Context, client.Object) []reconcile.Request = (*Reconciler)(nil).mapSecretToSources
