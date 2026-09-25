package main

import (
	"context"
	"github.com/go-logr/logr"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// This file covers the join between a Listener that writes a status condition
// during Start and the manager's own status write that follows it. Neither
// side is wrong alone; together they lost the condition, and no test on either
// side could see it.

// seamKindName is a test-only channel kind, registered once, whose Listener
// writes a status condition during Start — the shape the slack listener has
// (it writes ScopesValid from auth.test) reduced to the one fact this test is
// about. Registering a kind rather than hand-calling Start is what makes the
// test exercise startListener's real ordering.
const seamKindName = "scopeseamtest"

var registerSeamKindOnce sync.Once

func registerSeamKind(t *testing.T) {
	t.Helper()
	base, ok := registry.Get("fake")
	require.True(t, ok, "the fake kind must be registered to embed it")
	registerSeamKindOnce.Do(func() { registry.Register(&seamKind{Kind: base}) })
}

// seamKind delegates everything to the embedded fake kind except its name and
// its Listener, so it stays consistent with every registry-wide invariant
// (SupportsMonitoring vs NewMonitoringSender, FeatureSupport, …).
type seamKind struct{ channelkinds.Kind }

func (*seamKind) Name() string { return seamKindName }

func (*seamKind) NewListener(deps channelkinds.Deps) channelkinds.Listener {
	return &seamListener{deps: deps}
}

type seamListener struct{ deps channelkinds.Deps }

func (l *seamListener) Start(ctx context.Context) error {
	var live spiceboxv1alpha1.Channel
	if err := l.deps.K8sClient.Get(ctx, types.NamespacedName{
		Namespace: l.deps.Channel.Namespace, Name: l.deps.Channel.Name,
	}, &live); err != nil {
		return err
	}
	updated := live.DeepCopy()
	conditions.SetFalse(updated, &updated.Status.Conditions,
		spiceboxv1alpha1.ChannelConditionScopesValid,
		spiceboxv1alpha1.ReasonChannelMissingScopes,
		"bot token missing required scope(s): files:write.")
	return l.deps.K8sClient.Status().Patch(ctx, updated, client.MergeFrom(&live))
}

func (l *seamListener) Stop(context.Context) error { return nil }

// newClobberK8s builds a fake client with the status subresource registered for
// Channel, mirroring the real CRD (config/crds/…_channels.yaml declares
// subresources.status). Without it every Status().Patch is rejected as "not
// found" and this file would assert against writes that never happened.
func newClobberK8s(t *testing.T, objs ...runtime.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).
		Build()
}

// writeScopesValid stands in for what a listener does inside Start: a status
// patch of its own condition, made against the object as it was when the
// listener was constructed.
func writeScopesValid(t *testing.T, cli client.Client, ns, name string) {
	t.Helper()
	ctx := context.Background()
	var live spiceboxv1alpha1.Channel
	require.NoError(t, cli.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &live))
	updated := live.DeepCopy()
	conditions.SetFalse(updated, &updated.Status.Conditions,
		spiceboxv1alpha1.ChannelConditionScopesValid,
		spiceboxv1alpha1.ReasonChannelMissingScopes,
		"bot token missing required scope(s): files:write.")
	require.NoError(t, cli.Status().Patch(ctx, updated, client.MergeFrom(&live)))
}

func channelCondition(t *testing.T, cli client.Client, ns, name, condType string) *metav1.Condition {
	t.Helper()
	var got spiceboxv1alpha1.Channel
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &got))
	return conditions.Find(got.Status.Conditions, condType)
}

func clobberChannel() *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-channel"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           seamKindName,
			AgentClass:     "demo-agent",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "demo-creds"},
		},
	}
}

// TestPatchConnectedPreservesAConditionWrittenSinceTheTickList is the tight
// version of the defect: a JSON merge patch replaces status.conditions
// WHOLESALE, so patching from an object listed before another writer's write
// silently reverts that write.
func TestPatchConnectedPreservesAConditionWrittenSinceTheTickList(t *testing.T) {
	ch := clobberChannel()
	cli := newClobberK8s(t, ch)
	m := &channelManager{cli: cli, listeners: map[string]channelkinds.Listener{}, cancels: map[string]context.CancelFunc{}}

	// `ch` is the object as listed at the top of the reconcile tick. The
	// listener writes its condition after that snapshot was taken.
	writeScopesValid(t, cli, "demo-ns", "demo-channel")

	m.patchConnected(context.Background(), ch, true, spiceboxv1alpha1.ReasonChannelSocketAttached, "")

	assert.NotNil(t, channelCondition(t, cli, "demo-ns", "demo-channel", spiceboxv1alpha1.ChannelConditionScopesValid),
		"patchConnected must not erase a condition written since the tick's List")
	connected := channelCondition(t, cli, "demo-ns", "demo-channel", spiceboxv1alpha1.ChannelConditionConnected)
	require.NotNil(t, connected, "patchConnected must still write its own condition")
	assert.Equal(t, metav1.ConditionTrue, connected.Status)
}

// TestStartListenerLeavesTheListenersOwnConditionIntact spans the real manager
// path: startListener runs the Listener's Start (which writes ScopesValid) and
// then patchConnected, in that order, on every listener start — i.e. after
// every channelsd restart, for every Channel.
func TestStartListenerLeavesTheListenersOwnConditionIntact(t *testing.T) {
	registerSeamKind(t)
	ch := clobberChannel()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-creds"}}
	cli := newClobberK8s(t, ch, sec)
	m := &channelManager{cli: cli, listeners: map[string]channelkinds.Listener{}, cancels: map[string]context.CancelFunc{}}

	m.startListener(context.Background(), "demo-ns/demo-channel", ch)
	t.Cleanup(func() { m.stopListener(logr.Discard(), "demo-ns/demo-channel") })

	require.NotNil(t, m.listeners["demo-ns/demo-channel"], "the listener must have started for this test to prove anything")
	assert.NotNil(t, channelCondition(t, cli, "demo-ns", "demo-channel", spiceboxv1alpha1.ChannelConditionScopesValid),
		"the condition the listener wrote in Start must survive the manager's own status write")
	assert.NotNil(t, channelCondition(t, cli, "demo-ns", "demo-channel", spiceboxv1alpha1.ChannelConditionConnected),
		"Connected must still be written")
}
