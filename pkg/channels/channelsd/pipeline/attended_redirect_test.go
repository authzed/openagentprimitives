package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// Task 7 of the agent-builder delegation framework: while an `attended`
// delegation child is live, a human's turn on the ROOT's own channel is
// meant for the CHILD, not the root itself (spec §4). Deliver's own
// correlateSessions can never resolve the child directly -- buildChild
// (Task 5) deliberately stamps it with NEITHER LabelChannelName nor
// LabelChannelKey, exactly so the label-selector sweeps those labels feed
// keep seeing exactly one session per (channel, key): the root -- so every
// fixture here mirrors that shape: the child carries ONLY Task 5's
// LabelAttendedParentNamespace/Name watch pair, never the channel-
// correlation labels existingSession stamps.

// attendedChildWatching is a live `attended` delegation child shaped like
// buildChild actually provisions one: owner-refed to ownedBy (the
// SubagentRequest the initiative gate reads), spec.parent set to the
// watching parent, Task 5's watch-relationship labels, and NEITHER
// channel-correlation label -- so it is reachable only through the
// redirect this file exercises, never through correlateSessions directly.
// Its InputChannel binding otherwise mirrors existingSession's, since
// buildChild copies the root's own binding verbatim (destination fields)
// with its own NATSSubjectPrefix.
func attendedChildWatching(t *testing.T, name, parentNS, parentName, ownedBy, phase string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return attendedChildInNamespace(t, name, "default", parentNS, parentName, ownedBy, phase)
}

// attendedChildInNamespace is attendedChildWatching with the child's OWN
// namespace parameterized -- for the workshop topology, where the attended
// child lives in the workshop namespace W rather than the builder root's own
// namespace. Everything else (the watch labels, the owner ref, the copied
// binding) is identical: only where the child object lives differs.
func attendedChildInNamespace(t *testing.T, name, childNS, parentNS, parentName, ownedBy, phase string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	yes := true
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: childNS,
			Labels: map[string]string{
				spiceboxv1alpha1.LabelAttendedParentNamespace: parentNS,
				spiceboxv1alpha1.LabelAttendedParentName:      parentName,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
				Kind:       "SubagentRequest",
				Name:       ownedBy,
				Controller: &yes,
			}},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "ac1",
			Parent: &spiceboxv1alpha1.NamespacedRef{Namespace: parentNS, Name: parentName},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "c1", Kind: "fake", Key: "thread:C1:1",
				NATSSubjectPrefix: "ap.session." + childNS + "." + name,
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
}

// TestDeliver_LiveAttendedChild_RoutesHumanTurnToChildNotRoot is the brief's
// test (a): with a live attended child, a human's turn on the root's own
// channel is delivered to the CHILD -- not refused (subagentNotAddressable),
// and not appended to the root.
func TestDeliver_LiveAttendedChild_RoutesHumanTurnToChildNotRoot(t *testing.T) {
	ch := newChannel("c1")
	root := existingSession(t, "builder-root", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
	child := attendedChildWatching(t, "builder-root-child", root.Namespace, root.Name, "req-attended", spiceboxv1alpha1.AgentSessionPhaseRunning)
	sr := subagentRequestFor("req-attended", root.Name, spiceboxv1alpha1.SubagentModeAttended)

	p, az, mem, _, _ := newPipeline(t, ch, root, child, sr)

	dec := humanTurnInto(t, p, ch, "thread:C1:1", "let's try uploading a file")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
	// Two appends, not one: the turn lands on the CHILD (this test's own
	// claim), and Task 6's mirrorToAttendedParent unconditionally mirrors that
	// SAME turn onto the watching root too, once `active` is the child -- the
	// "parent sees every turn" guarantee composing with this task's redirect,
	// not a second, duplicate delivery of the turn itself.
	require.Len(t, mem.appends, 2, "the child's own turn, plus Task 6's unconditional mirror to the watching root")
	assert.Equal(t, child.Name, mem.appends[0].name, "the turn must land on the CHILD's own memory first")
	assert.Equal(t, root.Name, mem.appends[1].name, "the root sees the same turn via the existing attended-watch mirror")
	assert.Equal(t, child.Name, dec.Session.Name, "the decision must name the child as the delivered-to session")
	assert.Positive(t, az.checkCalls, "redirected or not, the ordinary interact check still runs")

	var gotRoot spiceboxv1alpha1.AgentSession
	require.NoError(t, p.K8s.Get(context.Background(), client.ObjectKey{Namespace: root.Namespace, Name: root.Name}, &gotRoot))
	assert.Empty(t, gotRoot.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
		"an ordinary, non-mentioning turn on the child must not wake the root -- it only sees, per Task 6")
}

// TestDeliver_NoLiveAttendedChild_RoutesToRootUnchanged is the brief's
// refusing/edge case: with NO live attended child, a human's turn to the
// root routes to the root exactly as before -- the non-attended path is
// byte-identical (this session carries no attended relationship at all, the
// same fixture TestDeliver_UndelegatedSession_IsNotGatedOnDelegationTerms
// uses).
func TestDeliver_NoLiveAttendedChild_RoutesToRootUnchanged(t *testing.T) {
	ch := newChannel("c1")
	root := existingSession(t, "builder-root", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, _, mem, _, _ := newPipeline(t, ch, root)

	dec := humanTurnInto(t, p, ch, "thread:C1:1", "hello")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
	require.Len(t, mem.appends, 1)
	assert.Equal(t, root.Name, mem.appends[0].name, "with nothing to redirect to, the turn lands on the root as before")
}

// TestDeliver_TerminalAttendedChild_RoutesBackToRoot is the redirect's own
// clearing: once the child reaches a terminal phase, liveAttendedChildOf no
// longer finds it, and a subsequent human turn on the root's channel routes
// to the root again -- with no separate "clear the redirect" step, since
// the redirect is re-resolved fresh on every inbound.
func TestDeliver_TerminalAttendedChild_RoutesBackToRoot(t *testing.T) {
	ch := newChannel("c1")
	root := existingSession(t, "builder-root", "", spiceboxv1alpha1.AgentSessionPhaseRunning)
	child := attendedChildWatching(t, "builder-root-child", root.Namespace, root.Name, "req-attended", spiceboxv1alpha1.AgentSessionPhaseSucceeded)
	sr := subagentRequestFor("req-attended", root.Name, spiceboxv1alpha1.SubagentModeAttended)

	p, _, mem, _, _ := newPipeline(t, ch, root, child, sr)

	dec := humanTurnInto(t, p, ch, "thread:C1:1", "how did it go?")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
	require.Len(t, mem.appends, 1)
	assert.Equal(t, root.Name, mem.appends[0].name, "a terminal child is no longer live, so the turn returns to the root")
}

// TestLiveAttendedChildOf pins the resolver in isolation: none, exactly one
// (live), a terminal one (excluded), and more than one live (refuses to
// guess, same as none).
func TestLiveAttendedChildOf(t *testing.T) {
	root := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "root-1"}}

	t.Run("no attended child at all: nil, no error", func(t *testing.T) {
		_, _, _, _, cli := newPipeline(t, root)
		got, err := liveAttendedChildOf(context.Background(), cli, root)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("exactly one live child: resolved", func(t *testing.T) {
		child := attendedChildWatching(t, "child-a", "default", "root-1", "req-a", spiceboxv1alpha1.AgentSessionPhaseRunning)
		_, _, _, _, cli := newPipeline(t, root, child)
		got, err := liveAttendedChildOf(context.Background(), cli, root)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "child-a", got.Name)
	})

	t.Run("only a terminal prior child: nil, not the resolved one", func(t *testing.T) {
		prior := attendedChildWatching(t, "child-b", "default", "root-1", "req-b", spiceboxv1alpha1.AgentSessionPhaseFailed)
		_, _, _, _, cli := newPipeline(t, root, prior)
		got, err := liveAttendedChildOf(context.Background(), cli, root)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("more than one live child: refuses to guess, nil not an error", func(t *testing.T) {
		c1 := attendedChildWatching(t, "child-c", "default", "root-1", "req-c", spiceboxv1alpha1.AgentSessionPhaseRunning)
		c2 := attendedChildWatching(t, "child-d", "default", "root-1", "req-d", spiceboxv1alpha1.AgentSessionPhaseRunning)
		_, _, _, _, cli := newPipeline(t, root, c1, c2)
		got, err := liveAttendedChildOf(context.Background(), cli, root)
		require.NoError(t, err)
		assert.Nil(t, got, "the one-attended-child-at-a-time invariant means this should never happen; refuse to pick one")
	})

	// The workshop case: an attended child provisioned inside a builder's
	// workshop lives in the workshop namespace W, NOT the builder root's own
	// namespace. The resolver's List is cluster-wide precisely so the human's
	// turn on the builder's channel can still reach it -- a namespace-scoped
	// List in the root's namespace would miss it entirely.
	t.Run("cross-namespace (workshop) child in namespace W: resolved", func(t *testing.T) {
		child := attendedChildInNamespace(t, "ws-child", "ws-builder-root-abc123", "default", "root-1", "req-ws", spiceboxv1alpha1.AgentSessionPhaseRunning)
		_, _, _, _, cli := newPipeline(t, root, child)
		got, err := liveAttendedChildOf(context.Background(), cli, root)
		require.NoError(t, err)
		require.NotNil(t, got, "a live attended child in the workshop namespace must be found despite the different namespace")
		assert.Equal(t, "ws-child", got.Name)
		assert.Equal(t, "ws-builder-root-abc123", got.Namespace, "the resolved child keeps its own (workshop) namespace, not the root's")
	})

	// The cluster-wide List must not over-match: the label pair names exactly
	// one (namespace, name) root, so a child in some other namespace watching a
	// DIFFERENT root is never returned for this one.
	t.Run("cross-namespace child watching a different root: not returned", func(t *testing.T) {
		otherRootsChild := attendedChildInNamespace(t, "other-child", "ws-other-root-xyz789", "default", "root-2", "req-other", spiceboxv1alpha1.AgentSessionPhaseRunning)
		_, _, _, _, cli := newPipeline(t, root, otherRootsChild)
		got, err := liveAttendedChildOf(context.Background(), cli, root)
		require.NoError(t, err)
		assert.Nil(t, got, "the label pair is unique to one root; a child watching root-2 must not resolve for root-1")
	})
}
