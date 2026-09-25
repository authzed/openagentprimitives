package sessionhold

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"

	// releaseBinding asks registry.DeliversToHuman whether a candidate
	// binding's channel kind is one a person reads, and an unregistered kind
	// is an error there rather than a fail-safe false. The operator registers
	// every kind in its own main; this test binary has to do the same or every
	// fixture binding below is unanswerable. Registered for BOTH test packages
	// in this directory — they link into one binary.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// key, newReconcilerWith, sessionWithBinding, and headlessChild are this
// file's own fixtures (package sessionhold, white-box, so releaseBinding is
// reachable) — controller_test.go's fixtures live in the black-box
// sessionhold_test package and are not visible here, so nothing is shadowed.

func key(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}

func newReconcilerWith(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(sch))
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(objs...).
		WithStatusSubresource(&v1.SessionHold{}, &v1.AgentSession{}).
		Build()
	return &Reconciler{Client: c}, c
}

func sessionWithBinding(t *testing.T, name string, b *v1.ChannelBinding) *v1.AgentSession {
	t.Helper()
	return &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec:       v1.AgentSessionSpec{Class: "demo-lead", InputChannel: b},
	}
}

// headlessChild has no InputChannel — the shape every Track 1a child has.
// parent == "" makes it a root, which is how the unroutable case is built.
func headlessChild(t *testing.T, name, parent string) *v1.AgentSession {
	t.Helper()
	s := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
		Spec:       v1.AgentSessionSpec{Class: "demo-coder"},
	}
	if parent != "" {
		s.Spec.Parent = &v1.NamespacedRef{Namespace: "ns", Name: parent}
	}
	return s
}

func TestReleaseBinding_HeadlessChildResolvesToTheRootsChannel(t *testing.T) {
	rootBinding := &v1.ChannelBinding{Name: "demo-channel", Kind: "fake", Key: "thread:C1:1"}

	r, c := newReconcilerWith(t,
		sessionWithBinding(t, "demo-root", rootBinding),
		headlessChild(t, "demo-child", "demo-root"),
	)
	_ = c

	var child v1.AgentSession
	require.NoError(t, r.Client.Get(context.Background(), key("ns", "demo-child"), &child))

	got, err := r.releaseBinding(context.Background(), &child)
	require.NoError(t, err)
	require.NotNil(t, got, "a headless child must resolve a binding through its parent, or its hold is a one-way door")
	assert.Equal(t, "demo-channel", got.Name)
}

func TestReleaseBinding_ConversationalChildSkipsItsAgentChannelForTheRootsHumanOne(t *testing.T) {
	// A `chat`/`task` child is NOT headless: it is bound to an `agent` Channel
	// whose far side is its parent session. Releasing a hold is a person's
	// decision, so the card must climb past that binding — resolving to it
	// would address the release card's approver at another agent.
	rootBinding := &v1.ChannelBinding{Name: "demo-channel", Kind: "slack", Key: "thread:C1:1"}
	childBinding := &v1.ChannelBinding{Name: "demo-child-agent-channel", Kind: "agent"}

	child := sessionWithBinding(t, "demo-child", childBinding)
	child.Spec.Parent = &v1.NamespacedRef{Namespace: "ns", Name: "demo-root"}

	r, _ := newReconcilerWith(t, sessionWithBinding(t, "demo-root", rootBinding), child)

	var got v1.AgentSession
	require.NoError(t, r.Client.Get(context.Background(), key("ns", "demo-child"), &got))

	binding, err := r.releaseBinding(context.Background(), &got)
	require.NoError(t, err)
	require.NotNil(t, binding)
	assert.Equal(t, "demo-channel", binding.Name, "must resolve the root's human channel")
	assert.NotEqual(t, childBinding.Name, binding.Name,
		"the child's own agent Channel leads to its parent agent, not to a person")
}

func TestReleaseBinding_RootWithNoBindingStillReturnsNil(t *testing.T) {
	r, _ := newReconcilerWith(t, headlessChild(t, "demo-orphan", ""))

	var sess v1.AgentSession
	require.NoError(t, r.Client.Get(context.Background(), key("ns", "demo-orphan"), &sess))

	got, err := r.releaseBinding(context.Background(), &sess)
	require.NoError(t, err, "a genuinely unroutable session is not an error")
	assert.Nil(t, got)
}

func TestReleaseBinding_StopsAtTheRootRatherThanLooping(t *testing.T) {
	// A cycle should be impossible (admission DAG-validates), but the walk must
	// terminate anyway: a controller that hangs takes the whole operator with it.
	r, _ := newReconcilerWith(t,
		headlessChild(t, "demo-a", "demo-b"),
		headlessChild(t, "demo-b", "demo-a"),
	)

	var sess v1.AgentSession
	require.NoError(t, r.Client.Get(context.Background(), key("ns", "demo-a"), &sess))

	_, err := r.releaseBinding(context.Background(), &sess)
	require.Error(t, err, "a lineage cycle must be reported, not spun on")
	assert.Contains(t, err.Error(), "cycle")
}

// TestPublishCard_HeadlessChild_RoutesThroughAndDoesNotPanic exercises
// publishCard end to end (via Reconcile, mirroring controller_test.go's own
// style) for a held HEADLESS child. This is the scenario Task 11 exists for:
// before this fix, sess.Spec.InputChannel == nil short-circuited publishCard
// before it ever reached ownerIdentity's sess.Spec.InputChannel.Kind read —
// so simply removing that guard without also threading the resolved binding
// through ownerIdentity would trade a silent no-op for a nil-pointer panic on
// the exact case this task unblocks. This test proves both: a card IS
// published, addressed via the released binding's Kind, and nothing panics.
func TestPublishCard_HeadlessChild_RoutesThroughAndDoesNotPanic(t *testing.T) {
	rootBinding := &v1.ChannelBinding{Name: "demo-channel", Kind: "fake", Key: "thread:C1:1"}
	root := sessionWithBinding(t, "demo-root", rootBinding)
	child := headlessChild(t, "demo-child", "demo-root")
	// A real subagentrequest-minted child carries its parent's started-by
	// annotations (subagentrequest's startedByAnnotations copies them at
	// creation) — set here so ownerIdentity has an addressable owner to
	// resolve, matching production shape rather than exercising an
	// unreachable "child with no owner at all" case this test isn't about.
	child.Annotations = map[string]string{
		v1.AnnotationStartedByExternalID: "U-OWNER",
		v1.AnnotationStartedByEmail:      "owner@example.com",
	}
	hold := &v1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "demo-hold"},
		Spec: v1.SessionHoldSpec{
			SessionRef: v1.NamespacedRef{Namespace: "ns", Name: "demo-child"},
			Reason:     "test trip",
			Source:     "test",
		},
		Status: v1.SessionHoldStatus{Phase: v1.SessionHoldPhaseActive},
	}

	sch := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(sch))
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(root, child, hold).
		WithStatusSubresource(&v1.SessionHold{}, &v1.AgentSession{}).
		Build()

	type recorded struct {
		subject string
		data    []byte
	}
	var recs []recorded
	r := &Reconciler{
		Client: c,
		NATSPublish: func(subject string, data []byte) error {
			recs = append(recs, recorded{subject, data})
			return nil
		},
	}

	require.NotPanics(t, func() {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key("ns", "demo-hold")})
		require.NoError(t, err, "Reconcile")
	})

	require.Len(t, recs, 1, "exactly one interaction_request published for the held headless child")
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(recs[0].data, &env))
	var payload channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &payload))
	assert.Equal(t, "demo-child", payload.AgentSessionRef.Name,
		"the card still addresses the held child itself, not the ancestor — Decide looks the hold up by this name")
	require.Len(t, payload.Audience.Approvers, 1)
	assert.Equal(t, "fake", string(payload.Audience.Approvers[0].Kind),
		"with no InputChannel of its own, the approver's channel kind falls back to the resolved ancestor binding")

	var gotHold v1.SessionHold
	require.NoError(t, c.Get(context.Background(), key("ns", "demo-hold"), &gotHold))
	assert.NotEmpty(t, gotHold.Status.InteractionRef, "InteractionRef recorded, proving the old no-op guard did not fire")
}
