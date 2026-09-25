//go:build integration

package agentclass_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// fakeStarterLinker records the LAST set it was asked to make exact, per class.
// Recording the set rather than a call count is the property under test: the
// write must carry the declared set, and an empty set must still be written
// (that is how a removed allowlist is deleted).
type fakeStarterLinker struct {
	mu   sync.Mutex
	last map[string][]string
	err  error
}

func (f *fakeStarterLinker) EnsureAgentClassStarters(_ context.Context, ns, name string, subjects []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		f.last = map[string][]string{}
	}
	f.last[ns+"/"+name] = append([]string(nil), subjects...)
	return f.err
}

func (f *fakeStarterLinker) lastFor(key string) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.last[key]
	return v, ok
}

func startersCondition(t *testing.T, env *testenv.Env, name string) *metav1.Condition {
	t.Helper()
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &got))
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == spiceboxv1alpha1.AgentClassConditionStartersLinked {
			return &got.Status.Conditions[i]
		}
	}
	return nil
}

func TestStarterLinks_WrittenForAValidClass(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	linker := &fakeStarterLinker{}
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client, StarterLinker: linker}
	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	})
	ac := newClass("sl-valid")
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
		AllowedStarters: []string{"user:abc123", "group:eng#member"},
	}}
	require.NoError(t, env.Client.Create(ctx, ac))
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "sl-valid"}})
	require.NoError(t, err)

	got, ok := linker.lastFor("default/sl-valid")
	require.True(t, ok, "the starter set must be written")
	assert.Equal(t, []string{"user:abc123", "group:eng#member"}, got)
	cond := startersCondition(t, env, "sl-valid")
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonStartersLinked, cond.Reason)
}

// Same ordering property the platform link has: an invalid class is exactly the
// one an admin may need to start to diagnose, so the well-formed subset of its
// starters is written BEFORE the validation gate.
func TestStarterLinks_WrittenEvenWhenTheClassIsInvalid(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	linker := &fakeStarterLinker{}
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client, StarterLinker: linker}
	ac := newClass("sl-invalid")
	ac.Spec.Model.APIKey.Name = "missing-secret-for-sl"
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
		AllowedStarters: []string{"user:abc123"},
	}}
	require.NoError(t, env.Client.Create(ctx, ac))
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "sl-invalid"}})
	got, ok := linker.lastFor("default/sl-invalid")
	require.True(t, ok, "written before the validation gate")
	assert.Equal(t, []string{"user:abc123"}, got)
}

// A class that DROPS its allowlist must have the tuples deleted, so the write
// still happens with an empty set.
func TestStarterLinks_EmptySetIsStillWritten(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	linker := &fakeStarterLinker{}
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client, StarterLinker: linker}
	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	})
	ac := newClass("sl-empty")
	require.NoError(t, env.Client.Create(ctx, ac))
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "sl-empty"}})
	require.NoError(t, err)
	got, ok := linker.lastFor("default/sl-empty")
	require.True(t, ok, "an empty set is a deletion and must reach SpiceDB")
	assert.Empty(t, got)
	cond := startersCondition(t, env, "sl-empty")
	require.NotNil(t, cond)
	assert.Equal(t, spiceboxv1alpha1.ReasonNoAllowedStarters, cond.Reason)
}

// Malformed entries are dropped from the write (validation reports them); a
// single bad entry must not withhold standing from the good ones.
func TestStarterLinks_MalformedEntriesAreNotWritten(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	linker := &fakeStarterLinker{}
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client, StarterLinker: linker}
	ac := newClass("sl-malformed")
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
		AllowedStarters: []string{"user:abc123", "bogus"},
	}}
	require.NoError(t, env.Client.Create(ctx, ac))
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "sl-malformed"}})
	got, ok := linker.lastFor("default/sl-malformed")
	require.True(t, ok)
	assert.Equal(t, []string{"user:abc123"}, got)
}

// Mirrors TestPlatformLink_TransientFailureRequeuesAndSaysSo: a transient
// write failure (SpiceDB unreachable, say) must requeue — swallowing it would
// leave the class permanently unstartable for anyone on its allowlist with no
// further retry — and the cause must land on the CR, not only in the log.
func TestStarterLinks_TransientFailureRequeuesAndSaysSo(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	linker := &fakeStarterLinker{err: errors.New("spicedb unreachable")}
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client, StarterLinker: linker}
	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	})
	ac := newClass("sl-transient")
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
		AllowedStarters: []string{"user:abc123"},
	}}
	require.NoError(t, env.Client.Create(ctx, ac))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "sl-transient"}})

	require.Error(t, err, "a transient starter-link failure must requeue")
	assert.Contains(t, err.Error(), "spicedb unreachable", "the cause must reach the caller, not be flattened")

	cond := startersCondition(t, env, "sl-transient")
	require.NotNil(t, cond, "the failure must be visible on the CR, not only in the returned error")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonStartersLinkFailed, cond.Reason)
}

// Mirrors TestPlatformLink_UnrepresentableNameIsPermanentAndDoesNotRequeue: a
// name outside SpiceDB's object_id charset can never be linked, and a
// Kubernetes name is immutable, so retrying is pointless — the condition is
// the report, and Reconcile must not requeue against it.
func TestStarterLinks_UnrepresentableNameIsPermanentAndDoesNotRequeue(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	linker := &fakeStarterLinker{err: fmt.Errorf("%w: agentclass %q", spicedb.ErrUnrepresentableObjectID, "default/sl.dotted")}
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client, StarterLinker: linker}
	_ = env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	})
	ac := newClass("sl-permanent")
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
		AllowedStarters: []string{"user:abc123"},
	}}
	require.NoError(t, env.Client.Create(ctx, ac))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "sl-permanent"}})

	require.NoError(t, err, "a PERMANENT starter-link failure must not requeue — nothing a retry can do will fix a name")

	cond := startersCondition(t, env, "sl-permanent")
	require.NotNil(t, cond, "the only report of a permanent failure IS the condition; it must be stamped")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonUnrepresentableClassName, cond.Reason,
		"the permanent case must be distinguishable from the transient one; an operator's remedy differs")
}
