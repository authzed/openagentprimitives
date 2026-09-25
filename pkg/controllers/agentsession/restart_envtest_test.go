//go:build integration

package agentsession_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lineage"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/tool_dispatch_snapshot"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

type recordingSnap struct{ restores []workspace.SnapshotHandle }

func (r *recordingSnap) Snapshot(_ context.Context, _ workspace.PVCRef, _ workspace.SnapshotHandle) error {
	return nil
}
func (r *recordingSnap) Restore(_ context.Context, h workspace.SnapshotHandle, _ workspace.PVCRef) error {
	r.restores = append(r.restores, h)
	return nil
}
func (r *recordingSnap) GC(_ context.Context, _ workspace.SnapshotHandle) error { return nil }

// Done probes report instant completion: envtest runs no Job
// controller, so a real probe would never observe JobComplete and the
// restart reconciler would requeue forever instead of finishing the
// fork sequence these tests assert on.
func (r *recordingSnap) SnapshotDone(_ context.Context, _ workspace.PVCRef, _ workspace.SnapshotHandle) (bool, error) {
	return true, nil
}
func (r *recordingSnap) RestoreDone(_ context.Context, _ workspace.SnapshotHandle, _ workspace.PVCRef) (bool, error) {
	return true, nil
}

// (Reuses `fakeGranter` already defined in restart_test.go — same package.)

// TestEnvtest_RestartFromHere_FullForkSequence covers the canonical
// happy path: a session at rest with PendingRestart triggers the
// reconciler to produce a child session with copied memory, lineage
// edges, SpiceDB participant writes, and PVC restores for the
// affected bundle.
func TestEnvtest_RestartFromHere_FullForkSequence(t *testing.T) {
	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 60*time.Second)
	defer cancel()

	env := testenv.Shared(t)
	ns := "default"

	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parent-restart", Namespace: ns,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo", Prompt: spiceboxv1alpha1.PromptSource{Inline: "hi"}},
	}
	require.NoError(t, env.Client.Create(ctx, parent))
	// restartStarterAllowed (start_gate.go) resolves the parent's class before
	// any fork mode proceeds; this class carries no allowlist, so the gate is
	// transparent to every assertion below.
	require.NoError(t, env.Client.Create(ctx, ungatedClass(ns, "demo")))
	parent.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		// A class-level policy — the shape channelsd snapshots and the SpiceDB
		// writer parses ("<type>:<id>#<relation>").
		AppliedInteractPermission: "group:eng#member",
		PendingRestart: &spiceboxv1alpha1.PendingRestart{
			CutTurnIndex: 2, NewUserText: "edited!", TriggeredBy: "user:alice",
			RequestedAt: metav1.NewTime(time.Unix(1, 0)), TargetSessionName: "parent-restart-fk-test",
		},
	}
	// Signed as channelsd would, once the object has its UID: the reconciler
	// refuses a marker it cannot attribute to the connector, and the digest
	// binds to the parent's namespace, name and UID.
	armSignedRestart(t, parent)
	require.NoError(t, env.Client.Status().Update(ctx, parent))

	// Seed memory: turns 0..4 + a tool_dispatch_snapshot at turn 4.
	mem := memory.NewLocal(inmem.NewBackend())
	parentScope := memory.Scope{Kind: "session", ID: ns + "/parent-restart"}
	appender := turn.NewAppender(mem, parentScope)
	for i := 0; i <= 4; i++ {
		require.NoError(t, appender.Append(ctx, memory.Turn{
			Index: i, Role: "user", CreatedAt: time.Unix(int64(i), 0),
			Content: []memory.ContentBlock{{Type: "text", Text: "u"}},
		}))
	}
	require.NoError(t, tool_dispatch_snapshot.Record(ctx, mem, parentScope, tool_dispatch_snapshot.Content{
		ToolUseID: "u_x", SpiceboxSession: "parent-restart-bundle", TurnIndex: 4, Sequence: 0, SessionUID: "uid-parent",
	}))

	snap := &recordingSnap{}
	g := &fakeGranter{}
	r := &agentsession.Reconciler{
		Client:            env.Client,
		RestartMemory:     mem,
		Snapshotter:       snap,
		AuthzGranter:      g,
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	proceed, _, err := r.ReconcileRestart(ctx, parent)
	require.NoError(t, err)
	assert.False(t, proceed, "restart returns proceed=false to short-circuit Reconcile")

	// Child session created with correct spec.
	var child spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "parent-restart-fk-test"}, &child))
	assert.Equal(t, "parent-restart", child.Spec.ForkedFrom)
	require.NotNil(t, child.Spec.ForkedAtTurn)
	assert.Equal(t, int32(2), *child.Spec.ForkedAtTurn)
	assert.Equal(t, "edited!", child.Spec.Prompt.Inline)

	// Memory copied through cut + inbox at index 3.
	childScope := memory.Scope{Kind: "session", ID: ns + "/parent-restart-fk-test"}
	childTurns, err := turn.NewAppender(mem, childScope).ReadAll(ctx)
	require.NoError(t, err)
	var indices []int
	var roles []string
	for _, tn := range childTurns {
		indices = append(indices, tn.Index)
		roles = append(roles, tn.Role)
	}
	assert.Equal(t, []int{0, 1, 2, 3}, indices, "0..2 copied + inbox at 3")
	// Last entry is the inbox turn.
	assert.Equal(t, "inbox", roles[len(roles)-1])

	// Lineage edges.
	out, err := lineage.OutEdges(ctx, mem, parentScope)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "parent-restart-fk-test", out[0].Peer)
	inEdges, err := lineage.InEdges(ctx, mem, childScope)
	require.NoError(t, err)
	require.Len(t, inEdges, 1)
	assert.Equal(t, "parent-restart", inEdges[0].Peer)

	// SpiceDB writes against the child ref.
	require.Len(t, g.startedBy, 1)
	assert.Contains(t, g.startedBy[0], "parent-restart-fk-test")
	require.Len(t, g.interactParts, 1)

	// Snapshot restore for the affected bundle (turn 4 dispatch).
	require.Len(t, snap.restores, 1)
	assert.Equal(t, 4, snap.restores[0].TurnIndex)

	// Parent status updated: SupersededBy + Phase=Succeeded + PendingRestart cleared.
	var updatedParent spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "parent-restart"}, &updatedParent))
	assert.Equal(t, "parent-restart-fk-test", updatedParent.Status.SupersededBy)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, updatedParent.Status.Phase)
	assert.Nil(t, updatedParent.Status.PendingRestart)

	_ = client.MergeFrom // pin import
}

// TestEnvtest_Takeover_InheritTransfersOwnershipAndSkipsForkGate covers the
// different-user takeover of a terminal (Failed) session: the child is owned by
// the NEW user (bob), inherits the full parent transcript, and — critically —
// is produced even though the fork gate would DENY (allow:false), proving
// takeover bypasses agentsession#fork (channelsd is the choke point).
func TestEnvtest_Takeover_InheritTransfersOwnershipAndSkipsForkGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 60*time.Second)
	defer cancel()

	env := testenv.Shared(t)
	ns := "default"

	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parent-takeover", Namespace: ns,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice",
				spiceboxv1alpha1.AnnotationStartedByExternalID:  "UALICE",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo", Prompt: spiceboxv1alpha1.PromptSource{Inline: "hi"}},
	}
	require.NoError(t, env.Client.Create(ctx, parent))
	// restartStarterAllowed (start_gate.go) resolves the parent's class before
	// any fork mode proceeds; this class carries no allowlist, so the gate is
	// transparent to every assertion below.
	require.NoError(t, env.Client.Create(ctx, ungatedClass(ns, "demo")))
	parent.Status = spiceboxv1alpha1.AgentSessionStatus{
		// A terminal, non-policy-halt failure — an ordinary state a different
		// user may take over and inherit.
		Phase:         spiceboxv1alpha1.AgentSessionPhaseFailed,
		FailureReason: "SessionExpired",
		// The class-level interact policy, in the only shape the writer accepts
		// (spicedb.ParseSubject requires "<type>:<id>#<relation>"; the schema's
		// `participant: user | group#member` admits group#member). It carries
		// forward onto the child — see the participant assertion below. The
		// per-user "user:alice" this fixture used to carry is a value neither
		// the AgentClass contract nor the SpiceDB writer accepts.
		AppliedInteractPermission: "group:eng#member",
		PendingRestart: &spiceboxv1alpha1.PendingRestart{
			Mode:               spiceboxv1alpha1.PendingRestartModeTakeover,
			NewUserText:        "bob takes over",
			TriggeredBy:        "user:bob",
			NewOwnerExternalID: "UBOB",
			InheritHistory:     true,
			RequestedAt:        metav1.NewTime(time.Unix(1, 0)),
			TargetSessionName:  "parent-takeover-tk-child",
		},
	}
	// Signed as channelsd would, once the object has its UID: the reconciler
	// refuses a marker it cannot attribute to the connector, and the digest
	// binds to the parent's namespace, name and UID.
	armSignedRestart(t, parent)
	require.NoError(t, env.Client.Status().Update(ctx, parent))

	// Seed the parent transcript: turns 0..2.
	mem := memory.NewLocal(inmem.NewBackend())
	parentScope := memory.Scope{Kind: "session", ID: ns + "/parent-takeover"}
	appender := turn.NewAppender(mem, parentScope)
	for i := 0; i <= 2; i++ {
		require.NoError(t, appender.Append(ctx, memory.Turn{
			Index: i, Role: "user", CreatedAt: time.Unix(int64(i), 0),
			Content: []memory.ContentBlock{{Type: "text", Text: "alice-history"}},
		}))
	}

	g := &fakeGranter{}
	r := &agentsession.Reconciler{
		Client:            env.Client,
		RestartMemory:     mem,
		Snapshotter:       &recordingSnap{},
		AuthzGranter:      g,
		ForkChecker:       fakeForkChecker{allow: false}, // would DENY a normal fork
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	proceed, _, err := r.ReconcileRestart(ctx, parent)
	require.NoError(t, err)
	assert.False(t, proceed, "takeover short-circuits Reconcile")

	// Child created and owned by BOB — not alice.
	var child spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "parent-takeover-tk-child"}, &child))
	assert.Equal(t, "user:bob", child.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID], "child owner canonical = bob")
	assert.Equal(t, "UBOB", child.Annotations[spiceboxv1alpha1.AnnotationStartedByExternalID], "child owner external = bob")
	assert.Equal(t, "bob takes over", child.Spec.Prompt.Inline)

	// Full transcript inherited: 0..2 copied + inbox at 3.
	childScope := memory.Scope{Kind: "session", ID: ns + "/parent-takeover-tk-child"}
	childTurns, err := turn.NewAppender(mem, childScope).ReadAll(ctx)
	require.NoError(t, err)
	var indices []int
	for _, tn := range childTurns {
		indices = append(indices, tn.Index)
	}
	assert.Equal(t, []int{0, 1, 2, 3}, indices, "ordinary takeover inherits the whole transcript + new inbox")

	// SpiceDB started_by granted to BOB (the child owner), never alice.
	require.Len(t, g.startedBy, 1)
	assert.Contains(t, g.startedBy[0], "parent-takeover-tk-child|bob", "started_by = the new owner")
	// interact = owner + started_by + participant - denied, so "alice must not
	// retain interact" is a claim about TWO slices. The participant half lands
	// in interactParts via TouchInteractParticipant — asserting alice's absence
	// from startedBy alone inspected a slice that structurally cannot hold it,
	// while this very fixture drives the parent's snapshot onto the child.
	require.Len(t, g.interactParts, 1)
	assert.Equal(t, ns+"/parent-takeover-tk-child|group:eng#member", g.interactParts[0],
		"the parent's class interact policy carries forward — and nothing else does")
	assert.NotContains(t, g.interactParts[0], "alice", "prior owner must not retain interact via participant")

	// Parent superseded; PendingRestart cleared. A Failed parent stays Failed.
	var updatedParent spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "parent-takeover"}, &updatedParent))
	assert.Equal(t, "parent-takeover-tk-child", updatedParent.Status.SupersededBy)
	assert.Nil(t, updatedParent.Status.PendingRestart)

	// Lineage recorded as a takeover.
	out, err := lineage.OutEdges(ctx, mem, parentScope)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "parent-takeover-tk-child", out[0].Peer)
}

// TestEnvtest_Takeover_PolicyHaltStartsFresh covers the policy-halt carve-out:
// a takeover with InheritHistory=false seeds ONLY the new user's message — the
// halted transcript is NOT copied into the new owner's session.
func TestEnvtest_Takeover_PolicyHaltStartsFresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 60*time.Second)
	defer cancel()

	env := testenv.Shared(t)
	ns := "default"

	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parent-halt", Namespace: ns,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice",
				spiceboxv1alpha1.AnnotationStartedByExternalID:  "UALICE",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo", Prompt: spiceboxv1alpha1.PromptSource{Inline: "hi"}},
	}
	require.NoError(t, env.Client.Create(ctx, parent))
	// restartStarterAllowed (start_gate.go) resolves the parent's class before
	// any fork mode proceeds; this class carries no allowlist, so the gate is
	// transparent to every assertion below.
	require.NoError(t, env.Client.Create(ctx, ungatedClass(ns, "demo")))
	parent.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase:         spiceboxv1alpha1.AgentSessionPhaseFailed,
		FailureReason: "ToolGuardHalt",
		PendingRestart: &spiceboxv1alpha1.PendingRestart{
			Mode:               spiceboxv1alpha1.PendingRestartModeTakeover,
			NewUserText:        "bob starts fresh",
			TriggeredBy:        "user:bob",
			NewOwnerExternalID: "UBOB",
			InheritHistory:     false, // policy-halt carve-out
			RequestedAt:        metav1.NewTime(time.Unix(1, 0)),
			TargetSessionName:  "parent-halt-tk-child",
		},
	}
	// Signed as channelsd would, once the object has its UID: the reconciler
	// refuses a marker it cannot attribute to the connector, and the digest
	// binds to the parent's namespace, name and UID.
	armSignedRestart(t, parent)
	require.NoError(t, env.Client.Status().Update(ctx, parent))

	// Seed a parent transcript that must NOT be inherited.
	mem := memory.NewLocal(inmem.NewBackend())
	parentScope := memory.Scope{Kind: "session", ID: ns + "/parent-halt"}
	appender := turn.NewAppender(mem, parentScope)
	for i := 0; i <= 2; i++ {
		require.NoError(t, appender.Append(ctx, memory.Turn{
			Index: i, Role: "user", CreatedAt: time.Unix(int64(i), 0),
			Content: []memory.ContentBlock{{Type: "text", Text: "halted-secret"}},
		}))
	}

	g := &fakeGranter{}
	r := &agentsession.Reconciler{
		Client:            env.Client,
		RestartMemory:     mem,
		Snapshotter:       &recordingSnap{},
		AuthzGranter:      g,
		ForkChecker:       fakeForkChecker{allow: false},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	proceed, _, err := r.ReconcileRestart(ctx, parent)
	require.NoError(t, err)
	assert.False(t, proceed)

	// Child transcript has ONLY the new user's inbox turn — no parent history.
	childScope := memory.Scope{Kind: "session", ID: ns + "/parent-halt-tk-child"}
	childTurns, err := turn.NewAppender(mem, childScope).ReadAll(ctx)
	require.NoError(t, err)
	require.Len(t, childTurns, 1, "policy-halt takeover seeds only the new message")
	assert.Equal(t, 0, childTurns[0].Index)
	assert.Equal(t, "inbox", childTurns[0].Role)
	for _, tn := range childTurns {
		for _, b := range tn.Content {
			assert.NotContains(t, b.Text, "halted-secret", "the halted transcript must not leak to the new owner")
		}
	}
}

func TestEnvtest_RestartFromHere_TriggeredByStatusPatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 60*time.Second)
	defer cancel()

	env := testenv.Shared(t)
	ns := "default"

	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parent2", Namespace: ns,
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo", Prompt: spiceboxv1alpha1.PromptSource{Inline: "hi"}},
	}
	require.NoError(t, env.Client.Create(ctx, parent))
	// restartStarterAllowed (start_gate.go) resolves the parent's class before
	// any fork mode proceeds; this class carries no allowlist, so the gate is
	// transparent to every assertion below.
	require.NoError(t, env.Client.Create(ctx, ungatedClass(ns, "demo")))
	parent.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		// A class-level policy — the shape channelsd snapshots and the SpiceDB
		// writer parses ("<type>:<id>#<relation>").
		AppliedInteractPermission: "group:eng#member",
	}
	// No marker yet — this test arms it below via the status PATCH, which is
	// channelsd's real entry point.
	require.NoError(t, env.Client.Status().Update(ctx, parent))

	// Seed memory.
	mem := memory.NewLocal(inmem.NewBackend())
	parentScope := memory.Scope{Kind: "session", ID: ns + "/parent2"}
	appender := turn.NewAppender(mem, parentScope)
	for i := 0; i <= 3; i++ {
		require.NoError(t, appender.Append(ctx, memory.Turn{
			Index: i, Role: "user", CreatedAt: time.Unix(int64(i), 0),
			Content: []memory.ContentBlock{{Type: "text", Text: "u"}},
		}))
	}

	// Simulate channelsd's status patch (the canonical entry point for
	// the reconciler).
	base := parent.DeepCopy()
	patched := parent.DeepCopy()
	patched.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
		CutTurnIndex:      1,
		NewUserText:       "via-status",
		TriggeredBy:       "user:alice",
		RequestedAt:       metav1.NewTime(time.Unix(1, 0)),
		TargetSessionName: "parent2-fk-test",
	}
	// Signed as channelsd would: the reconciler refuses a marker it cannot
	// attribute to the connector, and the digest binds to the parent's
	// namespace, name and UID (populated by the Create above).
	armSignedRestart(t, patched)
	require.NoError(t, env.Client.Status().Patch(ctx, patched, client.MergeFrom(base)))

	// Re-fetch and drive the reconciler.
	var fetched spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "parent2"}, &fetched))

	snap := &recordingSnap{}
	g := &fakeGranter{}
	r := &agentsession.Reconciler{
		Client:            env.Client,
		RestartMemory:     mem,
		Snapshotter:       snap,
		AuthzGranter:      g,
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}
	proceed, _, err := r.ReconcileRestart(ctx, &fetched)
	require.NoError(t, err)
	assert.False(t, proceed)

	// Verify child created.
	var child spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "parent2-fk-test"}, &child))
	assert.Equal(t, "parent2", child.Spec.ForkedFrom)
	require.NotNil(t, child.Spec.ForkedAtTurn)
	assert.Equal(t, int32(1), *child.Spec.ForkedAtTurn)
	assert.Equal(t, "via-status", child.Spec.Prompt.Inline)
}

// recordingSlotCopier records whether the fork carried the parent's bound
// instances onto the child.
type recordingSlotCopier struct {
	held      []authz.SlotBinding
	grantedTo string
	granted   []authz.SlotBinding
}

func (c *recordingSlotCopier) ListSlotGrants(_ context.Context, _, _ string) ([]authz.SlotBinding, error) {
	return c.held, nil
}

func (c *recordingSlotCopier) GrantSlots(_ context.Context, ns, name string, b []authz.SlotBinding, _ time.Time) error {
	c.grantedTo = ns + "/" + name
	c.granted = append(c.granted, b...)
	return nil
}

// TestEnvtest_Takeover_CarriesNoSlotGrantsToTheNewOwner is the security half of
// slot fork, driven through the real reconciler.
//
// A takeover hands the child to a DIFFERENT user, who becomes its owner. Copying
// the parent's slot grants would resolve slot_grant->interact + owner for that
// user on resources they never had standing on — handing them the previous
// owner's human-approved instance authority. And the agentsession#fork gate does
// not run for takeover (this fixture sets allow:false and the fork still
// proceeds), so nothing else would stop it.
func TestEnvtest_Takeover_CarriesNoSlotGrantsToTheNewOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 60*time.Second)
	defer cancel()

	env := testenv.Shared(t)
	ns := "default"

	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parent-takeover-slots", Namespace: ns,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice",
				spiceboxv1alpha1.AnnotationStartedByExternalID:  "UALICE",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo", Prompt: spiceboxv1alpha1.PromptSource{Inline: "hi"}},
	}
	require.NoError(t, env.Client.Create(ctx, parent))
	// restartStarterAllowed (start_gate.go) resolves the parent's class before
	// any fork mode proceeds; this class carries no allowlist, so the gate is
	// transparent to every assertion below.
	require.NoError(t, env.Client.Create(ctx, ungatedClass(ns, "demo")))
	parent.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase:                     spiceboxv1alpha1.AgentSessionPhaseFailed,
		FailureReason:             "SessionExpired",
		AppliedInteractPermission: "group:eng#member",
		PendingRestart: &spiceboxv1alpha1.PendingRestart{
			Mode:               spiceboxv1alpha1.PendingRestartModeTakeover,
			NewUserText:        "bob takes over",
			TriggeredBy:        "user:bob",
			NewOwnerExternalID: "UBOB",
			InheritHistory:     true,
			RequestedAt:        metav1.NewTime(time.Unix(1, 0)),
			TargetSessionName:  "parent-takeover-slots-child",
		},
	}
	armSignedRestart(t, parent)
	require.NoError(t, env.Client.Status().Update(ctx, parent))

	mem := memory.NewLocal(inmem.NewBackend())
	parentScope := memory.Scope{Kind: "session", ID: ns + "/parent-takeover-slots"}
	appender := turn.NewAppender(mem, parentScope)
	require.NoError(t, appender.Append(ctx, memory.Turn{
		Index: 0, Role: "user", CreatedAt: time.Unix(0, 0),
		Content: []memory.ContentBlock{{Type: "text", Text: "alice-history"}},
	}))

	// The parent holds an approved instance. Alice earned it; bob did not.
	copier := &recordingSlotCopier{held: []authz.SlotBinding{
		{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme")},
	}}
	r := &agentsession.Reconciler{
		Client:            env.Client,
		RestartMemory:     mem,
		Snapshotter:       &recordingSnap{},
		AuthzGranter:      &fakeGranter{},
		ForkChecker:       fakeForkChecker{allow: false}, // would DENY a normal fork
		DeniedLister:      fakeDeniedLister{},
		PublisherKeys:     testMarkerKeys,
		SlotGrantCopier:   copier,
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
	}

	_, _, err := r.ReconcileRestart(ctx, parent)
	require.NoError(t, err)

	// The child exists — takeover really did proceed past the denying gate.
	var child spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "parent-takeover-slots-child"}, &child))
	assert.Equal(t, "user:bob", child.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID])

	assert.Empty(t, copier.granted,
		"a takeover must carry NO slot grants: the new owner would inherit instance authority a human approved for someone else")
	assert.Empty(t, copier.grantedTo)
}

// TestEnvtest_Inherit_CarriesSlotGrantsToTheChild is the companion the
// carve-out test needs: without it, simply never copying would satisfy the
// takeover assertion and silently break every continuation.
//
// A slot grant names the session as its subject, so the child inherits none on
// its own — a continuation would lose every instance a human approved and start
// re-asking for values the user already granted.
func TestEnvtest_Inherit_CarriesSlotGrantsToTheChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 60*time.Second)
	defer cancel()

	env := testenv.Shared(t)
	ns := "default"

	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parent-inherit-slots", Namespace: ns,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice",
				spiceboxv1alpha1.AnnotationStartedByExternalID:  "UALICE",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo", Prompt: spiceboxv1alpha1.PromptSource{Inline: "hi"}},
	}
	require.NoError(t, env.Client.Create(ctx, parent))
	// restartStarterAllowed (start_gate.go) resolves the parent's class before
	// any fork mode proceeds; this class carries no allowlist, so the gate is
	// transparent to every assertion below.
	require.NoError(t, env.Client.Create(ctx, ungatedClass(ns, "demo")))
	parent.Status = spiceboxv1alpha1.AgentSessionStatus{
		Phase:                     spiceboxv1alpha1.AgentSessionPhaseFailed,
		FailureReason:             "SessionExpired",
		AppliedInteractPermission: "group:eng#member",
		PendingRestart: &spiceboxv1alpha1.PendingRestart{
			Mode:              spiceboxv1alpha1.PendingRestartModeInherit,
			NewUserText:       "alice continues",
			TriggeredBy:       "user:alice",
			InheritHistory:    true,
			RequestedAt:       metav1.NewTime(time.Unix(1, 0)),
			TargetSessionName: "parent-inherit-slots-child",
		},
	}
	armSignedRestart(t, parent)
	require.NoError(t, env.Client.Status().Update(ctx, parent))

	mem := memory.NewLocal(inmem.NewBackend())
	parentScope := memory.Scope{Kind: "session", ID: ns + "/parent-inherit-slots"}
	appender := turn.NewAppender(mem, parentScope)
	require.NoError(t, appender.Append(ctx, memory.Turn{
		Index: 0, Role: "user", CreatedAt: time.Unix(0, 0),
		Content: []memory.ContentBlock{{Type: "text", Text: "alice-history"}},
	}))

	copier := &recordingSlotCopier{held: []authz.SlotBinding{
		{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme")},
		{ResourceType: "http_target", ResourceID: authz.TrustedObjectID("hash-1")},
	}}
	r := &agentsession.Reconciler{
		Client:            env.Client,
		RestartMemory:     mem,
		Snapshotter:       &recordingSnap{},
		AuthzGranter:      &fakeGranter{},
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		PublisherKeys:     testMarkerKeys,
		SlotGrantCopier:   copier,
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
	}

	_, _, err := r.ReconcileRestart(ctx, parent)
	require.NoError(t, err)

	assert.Equal(t, ns+"/parent-inherit-slots-child", copier.grantedTo,
		"the same-owner continuation must receive the parent's bound instances")
	assert.ElementsMatch(t, copier.held, copier.granted)
}
