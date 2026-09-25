package sessionhold_test

// cascade_test.go covers cascading a hold across a delegation subtree and its
// release, PLUS TestReconcile_CascadeFails_StillPublishesCard, an extra case
// proving a permanently-failing cascade does not block the only release path
// a held session has.
//
// Every case here drives cascadeHold and releaseCascade THROUGH Reconcile and
// Decide, never by calling either directly — a sweep unit-tested by direct
// invocation would pass green while Reconcile/Decide never reach it in
// production. (mapSessionToAncestorHolds, the AgentSession watch's mapping
// function, has no Reconcile/Decide entry point of its own — controller-
// runtime's manager invokes it directly — so it is tested separately, by
// direct invocation, in cascade_watch_test.go.)

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories/sessionrelease"
	"github.com/authzed/openagentprimitives/pkg/controllers/sessionhold"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// treeSession builds an AgentSession in the shared "demo-ns" namespace,
// parented and delegation-labelled the way buildChild
// (pkg/controllers/subagentrequest/controller.go) stamps a real delegated
// child at creation time: a root carries no label at all (RootNameFor's own
// rule — a session with no label IS a root) and every descendant carries
// LabelDelegationRoot set to the tree's root name, mirroring
// pkg/apis/v1alpha1/agentsession_closure_test.go's identical "labelled"
// fixture shape.
func treeSession(name, parentName, root string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if parentName != "" {
		s.Spec.Parent = &spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: parentName}
	}
	if root != "" {
		s.Labels = map[string]string{spiceboxv1alpha1.LabelDelegationRoot: root}
	}
	return s
}

// holdOn builds a SessionHold naming sessionName, at the given phase — the
// cascade tests' own fixture, distinct from controller_test.go's activeHold
// (which is pinned to that file's single fixed session/hold pair).
func holdOn(name, sessionName, source, phase string) *spiceboxv1alpha1.SessionHold {
	return &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: sessionName},
			Reason:     "test trip",
			Source:     source,
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: phase},
	}
}

func reconcileNamed(t *testing.T, r *sessionhold.Reconciler, holdName string) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: holdName}})
}

func listHolds(t *testing.T, c client.Client) []spiceboxv1alpha1.SessionHold {
	t.Helper()
	var list spiceboxv1alpha1.SessionHoldList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(ns)), "List SessionHolds")
	return list.Items
}

func getHoldNamed(t *testing.T, c client.Client, name string) *spiceboxv1alpha1.SessionHold {
	t.Helper()
	var got spiceboxv1alpha1.SessionHold
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &got), "Get SessionHold %s", name)
	return &got
}

// approveDecisionFor mirrors controller_test.go's approveDecision, generalized
// to an arbitrary session — that file's version is pinned to the single fixed
// sessionName constant, which the cascade tests' multi-session trees don't fit.
func approveDecisionFor(sessionName, requestRef string) channelinteractions.Decision {
	return channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: ns, Name: sessionName},
		Payload: channelevents.InteractionDecisionPayload{
			Category:   sessionrelease.CategoryName,
			RequestRef: requestRef,
			ActionID:   "approve",
			Decider:    channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U-OWNER", Email: "owner@example.com"},
		},
	}
}

// --- Case 1: holding a root holds every descendant -----------------------

func TestCascade_HoldingRoot_HoldsEveryDescendant(t *testing.T) {
	root := treeSession("cas-root", "", "")
	child1 := treeSession("cas-child-1", "cas-root", "cas-root")
	child2 := treeSession("cas-child-2", "cas-root", "cas-root")
	grand := treeSession("cas-grandchild", "cas-child-1", "cas-root")

	rootHold := holdOn("cas-root-hold", "cas-root", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)

	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, _ := newFixture(t, mem, root, child1, child2, grand, rootHold)

	_, err := reconcileNamed(t, r, "cas-root-hold")
	require.NoError(t, err, "Reconcile")

	holds := listHolds(t, c)
	require.Len(t, holds, 4, "the originating hold plus one cascaded hold per descendant")

	wantSessions := map[string]bool{"cas-root": true, "cas-child-1": true, "cas-child-2": true, "cas-grandchild": true}
	for _, h := range holds {
		assert.True(t, wantSessions[h.Spec.SessionRef.Name], "unexpected hold naming %s", h.Spec.SessionRef.Name)
		delete(wantSessions, h.Spec.SessionRef.Name)

		if h.Name == "cas-root-hold" {
			continue // the originating hold itself, not a cascaded one
		}
		require.Len(t, h.OwnerReferences, 1, "cascaded hold %s must carry exactly one owner-ref", h.Name)
		assert.Equal(t, h.Spec.SessionRef.Name, h.OwnerReferences[0].Name,
			"a cascaded hold is owner-ref'd to ITS OWN session — not the root, not the originating hold")
		assert.Equal(t, "cas-root-hold", h.Labels[spiceboxv1alpha1.LabelCascadeOf])
		assert.Equal(t, "cascade/cas-root-hold", h.Spec.Source)
		assert.Contains(t, h.Spec.Reason, "cas-root",
			"the cascaded hold's Reason must name the originating session, so an operator reading a child's hold learns why without cross-referencing")
	}
	assert.Empty(t, wantSessions, "every descendant, and the root itself, must be covered exactly once")
}

// --- Case 2: holding a mid-tree node holds its subtree only --------------

func TestCascade_HoldingMidTreeNode_HoldsItsSubtreeOnly(t *testing.T) {
	root := treeSession("cas-root", "", "")
	child1 := treeSession("cas-child-1", "cas-root", "cas-root")
	child2 := treeSession("cas-child-2", "cas-root", "cas-root")
	grand := treeSession("cas-grandchild", "cas-child-1", "cas-root")

	midHold := holdOn("cas-mid-hold", "cas-child-1", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)

	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, _ := newFixture(t, mem, root, child1, child2, grand, midHold)

	_, err := reconcileNamed(t, r, "cas-mid-hold")
	require.NoError(t, err, "Reconcile")

	holds := listHolds(t, c)
	require.Len(t, holds, 2, "the mid hold plus one cascaded hold for its only descendant")

	named := map[string]bool{}
	for _, h := range holds {
		named[h.Spec.SessionRef.Name] = true
	}
	assert.True(t, named["cas-child-1"], "the mid node's own hold")
	assert.True(t, named["cas-grandchild"], "its descendant")
	assert.False(t, named["cas-root"], "the ancestor must be untouched")
	assert.False(t, named["cas-child-2"], "the sibling branch must be untouched")
}

// --- Case 3: a cascaded hold does not itself cascade ----------------------

func TestCascade_CascadedHold_DoesNotItselfCascade(t *testing.T) {
	root := treeSession("cas-root", "", "")
	child1 := treeSession("cas-child-1", "cas-root", "cas-root")
	grand := treeSession("cas-grandchild", "cas-child-1", "cas-root")

	rootHold := holdOn("cas-root-hold", "cas-root", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)

	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, _ := newFixture(t, mem, root, child1, grand, rootHold)

	_, err := reconcileNamed(t, r, "cas-root-hold")
	require.NoError(t, err, "Reconcile the originating hold")
	require.Len(t, listHolds(t, c), 3, "root hold + cascaded holds on child1 and grandchild")

	// Simulate what pkg/controllers/agentsession's OWN reconcileHold does to
	// every SessionHold it observes Active-worthy — out of this package's
	// scope, so simulated directly here — then reconcile the CASCADED hold
	// itself, the same way the operator's manager would once it actually goes
	// Active.
	cascadedChild1Name := "cascade-cas-root-hold-cas-child-1"
	cascaded := getHoldNamed(t, c, cascadedChild1Name)
	cascaded.Status.Phase = spiceboxv1alpha1.SessionHoldPhaseActive
	require.NoError(t, c.Status().Update(context.Background(), cascaded))

	_, err = reconcileNamed(t, r, cascadedChild1Name)
	require.NoError(t, err, "Reconcile the cascaded hold")

	assert.Len(t, listHolds(t, c), 3,
		"reconciling a cascaded hold must not fan out again — no second-generation holds, no duplicates")
}

// TestCascade_HoldingRoot_PublishesOnlyOneCardNotOnePerDescendant pins the
// whole-branch review's Finding 2: agentsession's OWN reconcileHold stamps
// Phase=Active on the first unreleased hold naming a session — including a
// CASCADED one — and once Active, this package's Reconcile falls straight
// through to publishCard for THAT hold too; cascadeHold's own label guard
// only ever stops a cascaded hold from cascading AGAIN, it says nothing about
// publishing. Holding a root with two descendants would therefore post
// THREE "Release this held session?" cards into the same thread, all
// addressed to the same owner — a cascaded hold's card is suppressed
// entirely instead: the originating hold's card is the one release path for
// the whole subtree (releaseCascade clears every cascaded hold on that
// single click).
//
// Needs a CHANNEL-BOUND root (unlike every other case in this file) —
// publishCard no-ops on binding == nil, so a channel-less fixture would pass
// this assertion vacuously whether or not the suppression guard exists.
func TestCascade_HoldingRoot_PublishesOnlyOneCardNotOnePerDescendant(t *testing.T) {
	root := treeSession("cas-root", "", "")
	root.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "slack", Key: "thread:C1:1"}
	root.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByExternalID: "U-OWNER",
		spiceboxv1alpha1.AnnotationStartedByEmail:      "owner@example.com",
	}
	child1 := treeSession("cas-child-1", "cas-root", "cas-root")
	child2 := treeSession("cas-child-2", "cas-root", "cas-root")
	// A real delegated child inherits the parent's started-by annotations
	// verbatim (subagentrequest's buildChild -> startedByAnnotations) — set
	// here so ownerIdentity can build an addressable approver for either
	// child's OWN hold if this fixture's suppression guard were ever wrong
	// and let one through, rather than the test failing on an unrelated
	// "no addressable identity" payload error instead of the property under
	// test.
	for _, child := range []*spiceboxv1alpha1.AgentSession{child1, child2} {
		child.Annotations = map[string]string{
			spiceboxv1alpha1.AnnotationStartedByExternalID: "U-OWNER",
			spiceboxv1alpha1.AnnotationStartedByEmail:      "owner@example.com",
		}
	}

	rootHold := holdOn("cas-root-hold", "cas-root", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)

	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, recs := newFixture(t, mem, root, child1, child2, rootHold)

	_, err := reconcileNamed(t, r, "cas-root-hold")
	require.NoError(t, err, "Reconcile the originating hold")
	require.Len(t, listHolds(t, c), 3, "root + 2 cascaded")
	require.Len(t, *recs, 1, "the originating hold's own card")

	// Simulate what pkg/controllers/agentsession's OWN reconcileHold does to
	// EVERY unreleased hold naming a session it observes worth holding —
	// cascaded holds included — then reconcile each cascaded hold the same
	// way the operator's manager would once it actually goes Active.
	cascadedNames := []string{"cascade-cas-root-hold-cas-child-1", "cascade-cas-root-hold-cas-child-2"}
	for _, name := range cascadedNames {
		cascaded := getHoldNamed(t, c, name)
		cascaded.Status.Phase = spiceboxv1alpha1.SessionHoldPhaseActive
		require.NoError(t, c.Status().Update(context.Background(), cascaded))

		_, err = reconcileNamed(t, r, name)
		require.NoError(t, err, "Reconcile the cascaded hold %s", name)
	}

	assert.Len(t, *recs, 1,
		"only the originating hold's card may ever be published -- a cascaded hold's card is suppressed entirely, not merely deduplicated")

	for _, name := range cascadedNames {
		cascaded := getHoldNamed(t, c, name)
		assert.Empty(t, cascaded.Status.InteractionRef,
			"a cascaded hold must never record its own InteractionRef -- it has no card of its own for Decide to match a click against")
	}
}

// TestCascade_ConversationalChild_SuppressedBecauseCascadedNotBecauseHeadless
// closes the gap every OTHER fixture in this file leaves open: every
// cascaded-hold case above cascades onto a CHANNEL-LESS descendant, which
// existed for every session before Track 1b. That leaves two explanations
// for the suppression indistinguishable — "a cascaded hold is suppressed
// because it is labelled cascaded" and "a cascaded hold is suppressed
// because its session has nothing to publish to" — and only the first is the
// actual mechanism (controller.go's isCascaded guard, ahead of publishCard).
//
// This fixture gives the cascaded child the shape a conversational
// (task/chat) delegation now provisions (subagentrequest's buildChild): its
// OWN `agent`-kind spec.inputChannel. If the suppression were coincidentally
// passing today only because a cascaded child was unroutable, handing it a
// real binding would surface a second card. It does not — the card stays
// suppressed on the label alone, exactly like a headless cascaded child.
func TestCascade_ConversationalChild_SuppressedBecauseCascadedNotBecauseHeadless(t *testing.T) {
	root := treeSession("cas-root", "", "")
	root.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "slack", Key: "thread:C1:1"}
	root.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByExternalID: "U-OWNER",
		spiceboxv1alpha1.AnnotationStartedByEmail:      "owner@example.com",
	}
	child := treeSession("cas-child-1", "cas-root", "cas-root")
	// The shape Task 8 now provisions for a task/chat delegation: a real
	// binding of its own, to an `agent`-kind Channel whose far side is its
	// parent — never a human-readable surface (see registry.DeliversToHuman /
	// releaseBinding's doc), but unlike a single_turn child, not NIL either.
	child.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "cas-child-1-agent", Kind: "agent", Key: "cas-root:cas-child-1"}
	child.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByExternalID: "U-OWNER",
		spiceboxv1alpha1.AnnotationStartedByEmail:      "owner@example.com",
	}

	rootHold := holdOn("cas-root-hold", "cas-root", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)

	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, recs := newFixture(t, mem, root, child, rootHold)

	_, err := reconcileNamed(t, r, "cas-root-hold")
	require.NoError(t, err, "Reconcile the originating hold")
	require.Len(t, listHolds(t, c), 2, "root + 1 cascaded")
	require.Len(t, *recs, 1, "the originating hold's own card")

	cascadedName := "cascade-cas-root-hold-cas-child-1"
	cascaded := getHoldNamed(t, c, cascadedName)
	cascaded.Status.Phase = spiceboxv1alpha1.SessionHoldPhaseActive
	require.NoError(t, c.Status().Update(context.Background(), cascaded))

	_, err = reconcileNamed(t, r, cascadedName)
	require.NoError(t, err, "Reconcile the cascaded hold on the conversational child")

	assert.Len(t, *recs, 1,
		"still exactly one card -- the cascaded child's own agent-kind binding does not earn it a card of its own; suppression is on the cascade label, not on whether the session has a surface")

	gotCascaded := getHoldNamed(t, c, cascadedName)
	assert.Empty(t, gotCascaded.Status.InteractionRef,
		"a cascaded hold never records its own InteractionRef, even when its session has a channel binding of its own to publish to")
}

// --- Case 4: releasing the originating hold releases the cascaded ones ---

// TestCascade_ReleasingOriginating_ReleasesCascadedHolds proves the
// anti-one-way-door property: releasing the root durably releases every
// cascaded hold (Phase, Determination, ReleasedBy) in the SAME Decide call.
// Nothing here deletes the cascaded CRs — they persist after release exactly
// like every OTHER released hold in this codebase (a manual hold, a
// tripper's), GC'd the ordinary way by their own owner-ref once their
// session is eventually deleted. An earlier version of releaseCascade paired
// this write with a confirm-then-delete cleanup step; it was removed because
// the confirmation window was itself a race that could delete a cascaded
// hold out from under a child one reconcile pass away from recording
// PhaseHeld — narrower than the original bug, but the same failure class.
// See cascade.go's releaseCascade doc for the full argument.
func TestCascade_ReleasingOriginating_ReleasesCascadedHolds(t *testing.T) {
	root := treeSession("cas-root", "", "")
	child1 := treeSession("cas-child-1", "cas-root", "cas-root")
	child2 := treeSession("cas-child-2", "cas-root", "cas-root")

	rootHold := holdOn("cas-root-hold", "cas-root", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)
	rootHold.Status.InteractionRef = "req-root"

	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, _ := newFixture(t, mem, root, child1, child2, rootHold)

	_, err := reconcileNamed(t, r, "cas-root-hold")
	require.NoError(t, err, "Reconcile the originating hold to cascade")
	require.Len(t, listHolds(t, c), 3, "root + 2 cascaded")

	cascadedNames := []string{"cascade-cas-root-hold-cas-child-1", "cascade-cas-root-hold-cas-child-2"}

	out, err := r.Decide(context.Background(), approveDecisionFor("cas-root", "req-root"))
	require.NoError(t, err, "Decide")
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)

	// Every cascaded hold is released, durably, in this same call — not
	// deferred, and never deleted.
	for _, name := range cascadedNames {
		h := getHoldNamed(t, c, name)
		assert.Equal(t, spiceboxv1alpha1.SessionHoldPhaseReleased, h.Status.Phase,
			"released immediately by Decide -- one approval clears the whole subtree")
		assert.NotEmpty(t, h.Status.ReleasedBy, "attributed to the same approver who released the originating hold")
	}
	assert.Len(t, listHolds(t, c), 3, "still 3 -- release never deletes a cascaded hold's CR")

	// The originating hold itself released too, same as any direct release.
	gotRoot := getHoldNamed(t, c, "cas-root-hold")
	assert.Equal(t, spiceboxv1alpha1.SessionHoldPhaseReleased, gotRoot.Status.Phase)
}

// --- Case 5: a hold created directly on a child is untouched -------------

func TestCascade_DirectHoldOnChild_UntouchedByParentRelease(t *testing.T) {
	root := treeSession("cas-root", "", "")
	child1 := treeSession("cas-child-1", "cas-root", "cas-root")
	child2 := treeSession("cas-child-2", "cas-root", "cas-root")

	rootHold := holdOn("cas-root-hold", "cas-root", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)
	rootHold.Status.InteractionRef = "req-root"

	// A hold a human placed directly on child2, independent of the root's
	// cascade — no LabelCascadeOf, because nothing cascaded it.
	manualHold := holdOn("cas-manual-hold", "cas-child-2", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)

	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, _ := newFixture(t, mem, root, child1, child2, rootHold, manualHold)

	_, err := reconcileNamed(t, r, "cas-root-hold")
	require.NoError(t, err, "Reconcile")

	_, err = r.Decide(context.Background(), approveDecisionFor("cas-root", "req-root"))
	require.NoError(t, err, "Decide")

	stillHere := getHoldNamed(t, c, "cas-manual-hold")
	assert.Equal(t, spiceboxv1alpha1.SessionHoldPhaseActive, stillHere.Status.Phase,
		"a hold created directly on a child, carrying no cascade label, must survive an unrelated parent's release — untouched, not released")
	assert.Empty(t, stillHere.Labels[spiceboxv1alpha1.LabelCascadeOf])
}

// --- Case 6: idempotent ----------------------------------------------------

func TestCascade_Idempotent_ReconcilingTwiceCreatesNoSecondSet(t *testing.T) {
	root := treeSession("cas-root", "", "")
	child1 := treeSession("cas-child-1", "cas-root", "cas-root")
	child2 := treeSession("cas-child-2", "cas-root", "cas-root")

	rootHold := holdOn("cas-root-hold", "cas-root", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)

	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, _ := newFixture(t, mem, root, child1, child2, rootHold)

	_, err := reconcileNamed(t, r, "cas-root-hold")
	require.NoError(t, err, "first Reconcile")
	require.Len(t, listHolds(t, c), 3)

	_, err = reconcileNamed(t, r, "cas-root-hold")
	require.NoError(t, err, "a second reconcile must not error on the deterministic names' AlreadyExists")

	assert.Len(t, listHolds(t, c), 3, "no duplicate holds from a second cascade pass")
}

// --- Extra: a failing cascade must not block the release card -----------

// TestReconcile_CascadeFails_StillPublishesCard proves the CRITICAL property
// fixed alongside the six cases above: a cascadeHold failure must never
// prevent publishCard from running. publishCard is what sets
// Status.InteractionRef, and Decide refuses any click whose RequestRef does
// not match it — so if a cascade failure blocked the card, a permanently-
// failing cascade (an over-length label value in production; a broken
// ancestor link here, since a fake client does not validate label values)
// would leave a frozen session, its pods already reaped, with no click able
// to release it at all.
//
// The trigger here is a "ghost" member of cas-root's own delegation closure:
// it carries LabelDelegationRoot (so ListClosure returns it, and cascadeHold
// attempts to walk it) but its parent reference is broken — mirrors
// pkg/apis/v1alpha1/agentsession_closure_test.go's identical fixture, the
// established way to make DescendantsOf fail without needing a real API
// server.
func TestReconcile_CascadeFails_StillPublishesCard(t *testing.T) {
	root := treeSession("cas-root", "", "")
	root.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "slack", Key: "thread:C1:1"}
	root.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByExternalID: "U-OWNER",
		spiceboxv1alpha1.AnnotationStartedByEmail:      "owner@example.com",
	}
	ghost := treeSession("cas-ghost", "missing-parent", "cas-root")

	rootHold := holdOn("cas-root-hold", "cas-root", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)

	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, recs := newFixture(t, mem, root, ghost, rootHold)

	_, err := reconcileNamed(t, r, "cas-root-hold")
	require.Error(t, err, "a cascade failure must surface (for the standard requeue-with-backoff), not be swallowed")
	assert.Contains(t, err.Error(), "cascade hold")

	// The card was STILL published -- a failed cascade must not lock a human
	// out of the only release path this hold has.
	require.Len(t, *recs, 1, "the release card is published even though the cascade failed")

	got := getHoldNamed(t, c, "cas-root-hold")
	assert.NotEmpty(t, got.Status.InteractionRef, "InteractionRef recorded -- Decide can still find and act on this hold")
	assert.Contains(t, got.Status.Determination, "cascade FAILED",
		"the failure is recorded on Status.Determination, where an operator inspecting the SessionHold will see it -- the card itself does not surface it")
}
