package v1alpha1_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// sess builds an AgentSession named name in "default", optionally parented to
// parent and optionally carrying an input-channel binding of the given channel
// kind. An empty bindingKind leaves the session headless.
func sess(t *testing.T, name, parent, bindingKind string) *v1.AgentSession {
	t.Helper()
	s := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
	if parent != "" {
		s.Spec.Parent = &v1.NamespacedRef{Namespace: "default", Name: parent}
	}
	if bindingKind != "" {
		s.Spec.InputChannel = &v1.ChannelBinding{Kind: bindingKind, Name: name + "-channel"}
	}
	return s
}

// deliversToHuman stands in for registry.DeliversToHuman, which this package
// may not import (pkg/apis depends on nothing else in the repo — that is why
// the predicate is a parameter in the first place). The answers mirror the
// registered kinds: "fake"/"slack" are surfaces a person reads, "agent" is
// another AgentSession, and an unregistered name is an error rather than a
// fail-safe false.
func deliversToHuman(kindName string) (bool, error) {
	switch kindName {
	case "fake", "slack":
		return true, nil
	case "agent":
		return false, nil
	}
	return false, fmt.Errorf("unknown kind %q", kindName)
}

func newReader(t *testing.T, objs ...*v1.AgentSession) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return b
}

func TestResolveHumanDirectedBinding_HeadlessLeaf_FindsTheAncestorsBinding(t *testing.T) {
	root := sess(t, "root", "", "fake")
	mid := sess(t, "mid", "root", "")
	leaf := sess(t, "leaf", "mid", "")
	c := newReader(t, root, mid, leaf).Build()

	got, err := v1.ResolveHumanDirectedBinding(context.Background(), c, leaf, deliversToHuman)
	require.NoError(t, err, "walking two hops to a bound root must succeed")
	require.NotNil(t, got, "the root's binding must be found from the leaf")
	assert.Equal(t, "fake", got.Binding.Kind)
	assert.Equal(t, "root-channel", got.Binding.Name)
}

func TestResolveHumanDirectedBinding_LeafWithAnAgentBinding_SkipsItForTheRootsHumanOne(t *testing.T) {
	// The conversational-subagent shape: the leaf IS bound — to an `agent`
	// Channel whose far side is its parent session — so a first-non-nil walk
	// would stop on it and hand a human's decision to another agent.
	root := sess(t, "root", "", "slack")
	mid := sess(t, "mid", "root", "")
	leaf := sess(t, "leaf", "mid", "agent")
	c := newReader(t, root, mid, leaf).Build()

	got, err := v1.ResolveHumanDirectedBinding(context.Background(), c, leaf, deliversToHuman)
	require.NoError(t, err)
	require.NotNil(t, got, "the walk must climb past the agent binding, not stop on it")
	assert.Equal(t, "slack", got.Binding.Kind)
	assert.Equal(t, "root-channel", got.Binding.Name)
}

func TestResolveHumanDirectedBinding_AgentBindingsAllTheWayUp_IsNilNilNotTheAgentChannel(t *testing.T) {
	// Every binding in the lineage leads to another agent. There is nobody to
	// ask, and the answer must be "nobody" — never a fallback onto the nearest
	// binding that happens to exist.
	root := sess(t, "root", "", "agent")
	leaf := sess(t, "leaf", "root", "agent")
	c := newReader(t, root, leaf).Build()

	got, err := v1.ResolveHumanDirectedBinding(context.Background(), c, leaf, deliversToHuman)
	require.NoError(t, err, "an all-agent lineage is a legitimate shape, not a walk failure")
	assert.Nil(t, got, "no human reads any binding in this lineage")
}

func TestResolveHumanDirectedBinding_GenuineRootWithNoBinding_IsNilNilNotAnError(t *testing.T) {
	root := sess(t, "root", "", "")
	c := newReader(t, root).Build()

	got, err := v1.ResolveHumanDirectedBinding(context.Background(), c, root, deliversToHuman)
	require.NoError(t, err, "a kubectl-driven root has nowhere to route to; that is not an error")
	assert.Nil(t, got)
}

func TestResolveHumanDirectedBinding_BrokenLineageLink_Errors(t *testing.T) {
	// leaf names a parent that does not exist: per no-silent-errors this must
	// surface, never be mistaken for "no binding".
	leaf := sess(t, "leaf", "ghost", "")
	c := newReader(t, leaf).Build()

	_, err := v1.ResolveHumanDirectedBinding(context.Background(), c, leaf, deliversToHuman)
	require.Error(t, err, "a failed Get while walking must surface")
	assert.Contains(t, err.Error(), "ghost")
}

func TestResolveHumanDirectedBinding_UnansweredKind_ErrorsAndNamesTheBinding(t *testing.T) {
	// A kind the predicate cannot answer for (in production: one this binary
	// never blank-imported). Fail closed AND loud — the caller drops either
	// way, and the error is what tells an operator to look at the wiring
	// rather than at the session tree.
	leaf := sess(t, "leaf", "", "not-registered")
	c := newReader(t, leaf).Build()

	_, err := v1.ResolveHumanDirectedBinding(context.Background(), c, leaf, deliversToHuman)
	require.Error(t, err, "an unanswerable channel kind must surface, not silently skip")
	assert.Contains(t, err.Error(), "leaf-channel", "the error must name the binding it could not judge")
	assert.Contains(t, err.Error(), "not-registered")
}

func TestResolveHumanDirectedBinding_NilPredicate_ErrorsRatherThanAcceptingAnything(t *testing.T) {
	// A nil predicate is a wiring bug, and the fail-open reading of it ("no
	// opinion, so take the first binding") is exactly the defect the parameter
	// exists to prevent.
	leaf := sess(t, "leaf", "", "agent")
	c := newReader(t, leaf).Build()

	got, err := v1.ResolveHumanDirectedBinding(context.Background(), c, leaf, nil)
	require.Error(t, err, "a missing predicate must be refused")
	assert.Nil(t, got)
}

func TestWalkAncestors_Cycle_ErrorsAtTheBoundRatherThanHanging(t *testing.T) {
	// a <-> b. Admission DAG-validates rosters, so reaching this means
	// something upstream already failed — but the walk must terminate loudly.
	a := sess(t, "a", "b", "")
	b := sess(t, "b", "a", "")
	c := newReader(t, a, b).Build()

	_, err := v1.ResolveRoot(context.Background(), c, a)
	require.Error(t, err, "a lineage cycle must terminate with an error, not loop")
	assert.Contains(t, err.Error(), "cycle")
}

func TestResolveRoot_ReturnsTheTopmostAncestor(t *testing.T) {
	root := sess(t, "root", "", "")
	mid := sess(t, "mid", "root", "")
	leaf := sess(t, "leaf", "mid", "")
	c := newReader(t, root, mid, leaf).Build()

	got, err := v1.ResolveRoot(context.Background(), c, leaf)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "root", got.Name, "ResolveRoot must climb to the topmost ancestor")
}

func TestResolveRoot_OnARootReturnsItself(t *testing.T) {
	root := sess(t, "root", "", "")
	c := newReader(t, root).Build()

	got, err := v1.ResolveRoot(context.Background(), c, root)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "root", got.Name, "a session with no parent is its own root")
}

// reachesSession stands in for registry.AllowsSessionCounterparty, on the same
// terms as deliversToHuman above: "agent" is the one registered kind whose far
// end is another AgentSession, and an unregistered name is an error.
func reachesSession(kindName string) (bool, error) {
	switch kindName {
	case "fake", "slack":
		return false, nil
	case "agent":
		return true, nil
	}
	return false, fmt.Errorf("unknown kind %q", kindName)
}

func TestIsDelegatedChild(t *testing.T) {
	cases := []struct {
		name    string
		session *v1.AgentSession
		want    bool
	}{
		{
			// The shape the whole thing exists for: a task/chat child, whose
			// agent_work_complete must complete the session rather than park it.
			name:    "delegated and bound to its parent: yes",
			session: sess(t, "child", "lead", "agent"),
			want:    true,
		},
		{
			// spec.parent alone is not enough. A single_turn child is headless
			// by construction, so it was never channel-attached and there is no
			// Idle-vs-Succeeded question to answer for it.
			name:    "delegated but headless (single_turn): no",
			session: sess(t, "child", "lead", ""),
			want:    false,
		},
		{
			// A binding that reaches another session is not enough either: this
			// session was not delegated to, so nothing is waiting on a result
			// from it.
			name:    "bound to an agent channel but not delegated: no",
			session: sess(t, "solo", "", "agent"),
			want:    false,
		},
		{
			// The case that must not regress: an ordinary channel-attached
			// session keeps parking Idle on agent_work_complete, because a
			// person may still say more.
			name:    "channel-attached to a human surface: no",
			session: sess(t, "root", "", "fake"),
			want:    false,
		},
		{
			// Delegated, but a person is on the other end of the binding, so
			// the conversation outlives the turn exactly as any human one does.
			name:    "delegated but bound to a human surface: no",
			session: sess(t, "child", "lead", "slack"),
			want:    false,
		},
		{
			name:    "nil session: no",
			session: nil,
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v1.IsDelegatedChild(tc.session, reachesSession)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestIsDelegatedChild_UnresolvableKindErrors pins that an unregistered
// binding kind is reported rather than answered.
//
// Both answers are silently wrong for one of the two shapes — false hangs a
// real delegation until its parent's call times out, true completes a session
// a person is still talking to — so the caller has to be told, and the runner
// treats it as a fatal wiring error at startup.
func TestIsDelegatedChild_UnresolvableKindErrors(t *testing.T) {
	_, err := v1.IsDelegatedChild(sess(t, "child", "lead", "not-registered"), reachesSession)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-registered")
}

// TestIsDelegatedChild_NilPredicateErrors pins that the predicate is required,
// mirroring ResolveHumanDirectedBinding: this package cannot answer the kind
// question itself, and defaulting to "no" would make every delegation hang.
func TestIsDelegatedChild_NilPredicateErrors(t *testing.T) {
	_, err := v1.IsDelegatedChild(sess(t, "child", "lead", "agent"), nil)
	require.Error(t, err)
}
