package skillsource

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bundlemem "github.com/authzed/openagentprimitives/pkg/tools/skillbundle/memory"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/skillfetch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reqNames flattens mapper output to "ns/name" strings for readable assertions.
func reqNames(reqs []reconcile.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Namespace+"/"+r.Name)
	}
	return out
}

// TestMapSecretToSources covers the watch that was missing: a credential Secret
// whose bytes change must re-enqueue the SkillSources that clone with it. Without
// it, rotating a PAT leaves a failing SkillSource serving its stale Ready=False
// until the (up to 1h) resync fires.
func TestMapSecretToSources(t *testing.T) {
	cases := []struct {
		name   string
		secret *corev1.Secret
		want   []string
	}{
		{
			name:   "secret backing the credential: enqueues its SkillSource",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pat", Namespace: "ns"}},
			want:   []string{"ns/src"},
		},
		{
			name:   "unrelated secret in the same namespace: no requests",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns"}},
			want:   []string{},
		},
		{
			name:   "same secret name in another namespace: no requests",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pat", Namespace: "elsewhere"}},
			want:   []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, id, sec := fixtures()
			c := newClient(t, src, id, sec)
			r := &Reconciler{Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New()}

			got := r.mapSecretToSources(context.Background(), tc.secret)
			assert.ElementsMatch(t, tc.want, reqNames(got))
		})
	}
}

// TestMapSecretToSourcesIgnoresAuthlessSource proves a public SkillSource (no
// spec.auth) is never enqueued by a Secret event — it has no credential to go
// stale, so waking it on every Secret write would be pure churn.
func TestMapSecretToSourcesIgnoresAuthlessSource(t *testing.T) {
	src, id, sec := fixtures()
	src.Spec.Auth = nil
	c := newClient(t, src, id, sec)
	r := &Reconciler{Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New()}

	got := r.mapSecretToSources(context.Background(), sec)
	assert.Empty(t, got)
}

// TestMapIdentityToSources covers the second half of the gap: repointing an
// AgentIdentity's credential at a different Secret changes which bytes the clone
// uses, and must re-enqueue every SkillSource authenticating through it.
func TestMapIdentityToSources(t *testing.T) {
	cases := []struct {
		name     string
		identity *v1.AgentIdentity
		want     []string
	}{
		{
			name:     "identity referenced by spec.auth: enqueues its SkillSource",
			identity: &v1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Name: "id", Namespace: "ns"}},
			want:     []string{"ns/src"},
		},
		{
			name:     "unreferenced identity: no requests",
			identity: &v1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Name: "other-id", Namespace: "ns"}},
			want:     []string{},
		},
		{
			name:     "same identity name in another namespace: no requests",
			identity: &v1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Name: "id", Namespace: "elsewhere"}},
			want:     []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, id, sec := fixtures()
			c := newClient(t, src, id, sec)
			r := &Reconciler{Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New()}

			got := r.mapIdentityToSources(context.Background(), tc.identity)
			assert.ElementsMatch(t, tc.want, reqNames(got))
		})
	}
}

// TestFailureRequeuesFasterThanSyncInterval pins the second half of the fix: a
// failing SkillSource must retry on the short failure cadence, not the full
// (default 1h) success cadence. The live incident this covers sat ~50 minutes on
// a stale git-401 after the PAT behind it had already been repaired.
func TestFailureRequeuesFasterThanSyncInterval(t *testing.T) {
	src, id, sec := fixtures()
	c := newClient(t, src, id, sec)
	r := &Reconciler{
		Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New(),
		SyncInterval: time.Hour,
		Fetcher:      &skillfetch.Fake{Err: errors.New("authentication required: Invalid username or token")},
	}

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: srcKey})
	require.NoError(t, err, "fetch failures surface as conditions, not returned errors")

	assert.Equal(t, failureRetryInterval, res.RequeueAfter,
		"a failing fetch must retry on the failure cadence, not the 1h success cadence")
	assert.Less(t, res.RequeueAfter, time.Hour)
}

// TestSuccessRequeuesOnSyncInterval guards the other direction: the short retry
// applies to failures only, so a healthy SkillSource is not re-polled every
// couple of minutes.
func TestSuccessRequeuesOnSyncInterval(t *testing.T) {
	src, id, sec := fixtures()
	c := newClient(t, src, id, sec)
	r := &Reconciler{
		Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New(),
		SyncInterval: time.Hour,
		Fetcher:      &skillfetch.Fake{Result: skillfetch.Result{SHA: "abc123"}},
	}

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: srcKey})
	require.NoError(t, err)
	assert.Equal(t, time.Hour, res.RequeueAfter)
}

// TestFailureRetryNeverSlowsAFastPoller ensures the clamp is a floor on
// frequency, not a ceiling: a SkillSource that asked for a 30s poll keeps it on
// the failure path rather than being slowed to the 2m failure cadence.
func TestFailureRetryNeverSlowsAFastPoller(t *testing.T) {
	src, id, sec := fixtures()
	src.Spec.Sync.Interval = metav1.Duration{Duration: 30 * time.Second}
	c := newClient(t, src, id, sec)
	r := &Reconciler{
		Client: c, SecretReader: newSecretReader(c), BundleStore: bundlemem.New(),
		Fetcher: &skillfetch.Fake{Err: errors.New("boom")},
	}

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: srcKey})
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, res.RequeueAfter,
		"a spec interval shorter than the failure cadence must win")
}

// compile-time guard: the mappers must satisfy handler.MapFunc's shape.
var (
	_ func(context.Context, client.Object) []reconcile.Request = (*Reconciler)(nil).mapSecretToSources
	_ func(context.Context, client.Object) []reconcile.Request = (*Reconciler)(nil).mapIdentityToSources
)
