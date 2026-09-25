package agentsession_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// noopSnap is a Snapshotter stub for the plain (non-integration) unit
// build. The integration-tagged restart_envtest_test.go has its own
// recordingSnap; this file can't see it across build tags.
type noopSnap struct{}

func (noopSnap) Snapshot(_ context.Context, _ workspace.PVCRef, _ workspace.SnapshotHandle) error {
	return nil
}
func (noopSnap) Restore(_ context.Context, _ workspace.SnapshotHandle, _ workspace.PVCRef) error {
	return nil
}
func (noopSnap) GC(_ context.Context, _ workspace.SnapshotHandle) error { return nil }

// Done probes report instant completion — this stub launches no Jobs,
// and the ordering test asserts the seed/create sequence, not the copy.
func (noopSnap) SnapshotDone(_ context.Context, _ workspace.PVCRef, _ workspace.SnapshotHandle) (bool, error) {
	return true, nil
}
func (noopSnap) RestoreDone(_ context.Context, _ workspace.SnapshotHandle, _ workspace.PVCRef) (bool, error) {
	return true, nil
}

// TestReconcileRestart_SeedsChildMemoryBeforeChildSessionExists pins the
// ordering invariant that keeps the restart fork from racing the child's
// runner.
//
// A forked child's memory must be fully seeded (parent prefix copied +
// the edited inbox turn appended) BEFORE the child AgentSession CR is
// created — because the CR is what lets a runner start, and a
// zero-latency runner (the in-process e2e factory; under load, a real
// pod) reads its memory, finds it empty, and writes its own turn-0 from
// Spec.Prompt. Since `turn` is an append-only kind with deterministic IDs
// (turn-000000-inbox), the reconciler's prefix copy then collides on that
// ID with ErrAppendOnlyConflict and the whole fork aborts — leaving the
// child with no inbox turn and no lineage edges (the symptom that made
// TestRestartFromHere_FullFlow flaky).
//
// The interceptor captures the child memory state at the exact moment the
// child CR is created; if seeding happens after creation, the assertion
// fails deterministically.
func TestReconcileRestart_SeedsChildMemoryBeforeChildSessionExists(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	const ns = "default"
	const parentName = "parent-restart"
	const childName = parentName + "-fk-test"

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	mem := memory.NewLocal(inmem.NewBackend())
	parentScope := memory.Scope{Kind: "session", ID: ns + "/" + parentName}
	childScope := memory.Scope{Kind: "session", ID: ns + "/" + childName}

	// Parent transcript: a single turn-0 inbox ("first question") — the
	// prefix the fork copies into the child at cut turn 0.
	require.NoError(t, turn.NewAppender(mem, parentScope).Append(ctx, memory.Turn{
		Index: 0, Role: "inbox", CreatedAt: time.Unix(0, 0),
		Content: []memory.ContentBlock{{Type: "text", Text: "first question"}},
	}))

	childInboxSeededAtCreate := false
	sawChildCreate := false

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if s, ok := obj.(*spiceboxv1alpha1.AgentSession); ok && s.Name == childName {
				sawChildCreate = true
				turns, _ := turn.NewAppender(mem, childScope).ReadAll(ctx)
				for _, tn := range turns {
					if tn.Role == "inbox" {
						childInboxSeededAtCreate = true
					}
				}
			}
			return cl.Create(ctx, obj, opts...)
		},
	})

	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: parentName, Namespace: ns,
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "demo",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "first question"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			// A class-level policy — the shape channelsd snapshots and the SpiceDB
			// writer parses ("<type>:<id>#<relation>").
			AppliedInteractPermission: "group:eng#member",
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				CutTurnIndex: 0, NewUserText: "edited question", TriggeredBy: "user:alice",
				RequestedAt: metav1.NewTime(time.Unix(1, 0)), TargetSessionName: childName,
			},
		},
	}
	armSignedRestart(t, parent)
	// The parent must exist in the client so the reconciler's status
	// patches (supersede, clear PendingRestart) resolve.
	require.NoError(t, base.Create(ctx, parent.DeepCopy()))
	// restartStarterAllowed (start_gate.go) resolves the parent's class before
	// any fork mode proceeds; this class carries no allowlist, so the gate is
	// transparent and the ordering assertion below is unaffected.
	require.NoError(t, base.Create(ctx, ungatedClass(ns, "demo")))

	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     mem,
		Snapshotter:       noopSnap{},
		AuthzGranter:      &fakeGranter{},
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	_, _, err := r.ReconcileRestart(ctx, parent)
	require.NoError(t, err, "fork must complete without an append-only conflict")

	require.True(t, sawChildCreate, "child session must have been created")
	assert.True(t, childInboxSeededAtCreate,
		"child memory inbox turn must be seeded BEFORE the child CR is created — "+
			"otherwise the child runner races the prefix copy and the fork aborts")

	// End state: the child carries the edited inbox turn.
	childTurns, err := turn.NewAppender(mem, childScope).ReadAll(ctx)
	require.NoError(t, err)
	sawEditedInbox := false
	for _, tn := range childTurns {
		if tn.Role == "inbox" && len(tn.Content) > 0 && tn.Content[0].Text == "edited question" {
			sawEditedInbox = true
		}
	}
	assert.True(t, sawEditedInbox, "child memory must contain the edited inbox turn")
}
