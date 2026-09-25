//go:build integration

// pkg/controllers/agentclass/platformlink_test.go — the agentclass#platform
// link, which is the LOAD-BEARING half of agentclass#start_session.
//
// agentclass#starter ships unpopulated, so this one tuple is the only thing
// making start_session satisfiable by anyone. Its absence is silent: the
// schema still compiles, the dashboard still renders, and the browser's agent
// picker is simply empty with nothing saying why. Every test here exists
// because that failure mode has no other alarm.
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

// fakeLinker records every (ns, name) it was asked to link and can be made to
// fail. It records rather than only answering because the property under test
// is that the write HAPPENS — for the specific classes, on paths where the
// class is invalid — and an implementation that silently skipped it would
// return the same nil either way.
type fakeLinker struct {
	mu   sync.Mutex
	seen []string
	err  error
}

func (f *fakeLinker) EnsureAgentClassPlatform(_ context.Context, ns, name string) error {
	f.mu.Lock()
	f.seen = append(f.seen, ns+"/"+name)
	f.mu.Unlock()
	return f.err
}

func (f *fakeLinker) linked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// linkCondition returns the PlatformLinked condition, or nil when absent.
func linkCondition(t *testing.T, env *testenv.Env, name string) *metav1.Condition {
	t.Helper()
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: name}, &got), "Get AgentClass")
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == spiceboxv1alpha1.AgentClassConditionPlatformLinked {
			return &got.Status.Conditions[i]
		}
	}
	return nil
}

func TestPlatformLink_WrittenForAValidClass(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	linker := &fakeLinker{}
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client, PlatformLinker: linker}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	}
	_ = env.Client.Create(ctx, sec) // shared env: may already exist
	ac := newClass("pl-valid")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pl-valid"}})
	require.NoError(t, err, "Reconcile")

	assert.Contains(t, linker.linked(), "default/pl-valid",
		"the link must be written; without it this class can never appear in the browser's agent picker")

	cond := linkCondition(t, env, "pl-valid")
	require.NotNil(t, cond, "a successful link must be reported as a condition, not silently")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPlatformLinked, cond.Reason)
}

// This is the property the whole ordering exists for. An AgentClass that fails
// validation is exactly the one an admin may need to start a session of in
// order to diagnose it, so the link must NOT be gated on the class being
// healthy. A reconcile that returned early on invalid would leave the class
// permanently unstartable while looking, from the picker, indistinguishable
// from one nobody has access to.
func TestPlatformLink_WrittenEvenWhenTheClassIsInvalid(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	linker := &fakeLinker{}
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client, PlatformLinker: linker}

	// No Secret for this class -> Valid=False/SecretMissing, an early return.
	ac := newClass("pl-invalid")
	ac.Spec.Model.APIKey.Name = "missing-secret-for-pl"
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass without its secret")

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pl-invalid"}})

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "pl-invalid"}, &got))
	require.True(t,
		conditionReason(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, spiceboxv1alpha1.ReasonSecretMissing),
		"precondition: this fixture must actually be invalid, or the test proves nothing; got %+v", got.Status.Conditions)

	assert.Contains(t, linker.linked(), "default/pl-invalid",
		"the link must be written BEFORE the validation gate — an invalid class is still one an admin may need to start")
}

func TestPlatformLink_TransientFailureRequeuesAndSaysSo(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	linker := &fakeLinker{err: errors.New("spicedb unreachable")}
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client, PlatformLinker: linker}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	}
	_ = env.Client.Create(ctx, sec)
	ac := newClass("pl-transient")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pl-transient"}})

	require.Error(t, err, "a transient link failure must requeue; swallowing it leaves the picker empty forever")
	assert.Contains(t, err.Error(), "spicedb unreachable", "the cause must reach the caller, not be flattened")

	cond := linkCondition(t, env, "pl-transient")
	require.NotNil(t, cond, "the failure must be visible on the CR, not only in the returned error")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonPlatformLinkFailed, cond.Reason)
}

// A name outside SpiceDB's object_id charset can NEVER be linked, and a
// Kubernetes name is immutable — so retrying is pointless and returning an
// error would requeue forever against a condition no retry can change. The
// condition is the report instead.
func TestPlatformLink_UnrepresentableNameIsPermanentAndDoesNotRequeue(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	linker := &fakeLinker{err: fmt.Errorf("%w: agentclass %q", spicedb.ErrUnrepresentableObjectID, "default/pl.dotted")}
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client, PlatformLinker: linker}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	}
	_ = env.Client.Create(ctx, sec)
	ac := newClass("pl-permanent")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pl-permanent"}})

	require.NoError(t, err, "a PERMANENT link failure must not requeue — nothing a retry can do will fix a name")

	cond := linkCondition(t, env, "pl-permanent")
	require.NotNil(t, cond, "the only report of a permanent failure IS the condition; it must be stamped")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonUnrepresentableClassName, cond.Reason,
		"the permanent case must be distinguishable from the transient one; an operator's remedy differs")
}

// A nil linker is a binary-wiring fact (local dev, a test fixture), identical
// for every AgentClass in the process. Stamping PlatformLinked=False onto every
// CR would be status noise about the operator, not about the object.
func TestPlatformLink_NilLinkerStampsNoConditionAndDoesNotFail(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client} // PlatformLinker deliberately unset

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk-ant-test")},
	}
	_ = env.Client.Create(ctx, sec)
	ac := newClass("pl-nil-linker")
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pl-nil-linker"}})
	require.NoError(t, err, "an unwired linker must not fail the reconcile")

	assert.Nil(t, linkCondition(t, env, "pl-nil-linker"),
		"no condition at all — a wiring fact belongs in the log, not on every CR's status")
}
