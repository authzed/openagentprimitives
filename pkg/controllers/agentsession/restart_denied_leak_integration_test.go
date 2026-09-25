//go:build integration

package agentsession_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// restartLeakTestSource identifies this file's own fixture writes (touchRel
// below) to the relsource guard — the same "fixture writer that owns
// nothing" shape as test/e2e/testhelpers.go's E2EHarnessSource. Claims
// deliberately empty: this file seeds arbitrary tuples (group membership,
// a memory_entry's session link) that no in-tree writer claims, so an
// empty-claim source never collides with a real one. Registered + marked
// complete here (rather than blank-importing
// pkg/authz/spicedb/relsource/imports) because this test cannot link that
// bundle without pulling in the slack channel kind's schema contribution —
// same reasoning bootstrap_validation_test.go documents for its own
// package-local relsource.MarkComplete().
var restartLeakTestSource = relsource.Source{Name: "agentsession-restart-leak-integration-test"}

func init() {
	relsource.Register(restartLeakTestSource)
	relsource.MarkComplete()
}

// uniqLeak returns a per-test-unique object id so parallel runs against the
// shared SpiceDB container do not collide.
func uniqLeak(t *testing.T, prefix string) string {
	t.Helper()
	var buf [8]byte
	_, err := rand.Read(buf[:])
	require.NoError(t, err, "rand.Read")
	return prefix + hex.EncodeToString(buf[:])
}

// touchRel TOUCH-writes one relationship resType:resID#relation@subjType:subjID
// (with an optional subject relation) against real SpiceDB, through a writer
// bound to restartLeakTestSource — (*spicedb.Client).WriteRelationships (the
// arbitrary-request pass-through) was removed as part of this branch's
// relsource guard, so an unguarded fixture writer no longer exists; every
// caller, including this one, goes through Client.Writer(src) now.
func touchRel(ctx context.Context, t *testing.T, c *spicedb.Client, resType, resID, relation, subjType, subjID, subjRel string) {
	t.Helper()
	subj := &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: subjType, ObjectId: subjID}}
	if subjRel != "" {
		subj.OptionalRelation = subjRel
	}
	_, err := c.Writer(restartLeakTestSource).WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: resType, ObjectId: resID},
				Relation: relation,
				Subject:  subj,
			},
		}},
	})
	require.NoErrorf(t, err, "TOUCH %s:%s#%s@%s:%s", resType, resID, relation, subjType, subjID)
}

// checkPerm answers resType:resID#permission@user:canonicalID, fully consistent.
func checkPerm(ctx context.Context, t *testing.T, c *spicedb.Client, resType, resID, permission, canonicalID string) bool {
	t.Helper()
	resp, err := c.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Resource:    &v1.ObjectReference{ObjectType: resType, ObjectId: resID},
		Permission:  permission,
		Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID}},
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
	})
	require.NoErrorf(t, err, "check %s:%s#%s@user:%s", resType, resID, permission, canonicalID)
	return resp.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
}

// TestReconcileRestart_InheritParentDeniedUserCannotReadChildTranscript is the
// direct security assertion coupled to the denied-copy-before-participant-grant
// ordering fix: after an inherit fork with a BROAD interact policy (a group
// subject-set), a user who was in the parent's denied set must have NO interact
// on the CHILD and be 403'd on the child's inherited transcript
// (memory_entry#read = session->read_transcript) — even though the broad policy
// was carried forward and the denied user is a member of that group.
//
// This runs against real SpiceDB (the production schema) so it proves the actual
// permission semantics, not a fake's approximation. With the ordering fix the
// denied blocklist is written before the participant grant, so interact is never
// briefly true; the assertion holds deterministically.
func TestReconcileRestart_InheritParentDeniedUserCannotReadChildTranscript(t *testing.T) {
	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 30*time.Second)
	defer cancel()

	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	spdb, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = spdb.Close() })

	const ns = "integration-test"
	parentName := uniqLeak(t, "parent-")
	childName := parentName + "-ic1"
	group := uniqLeak(t, "eng-") // group object id
	owner := "alice"             // canonical user id (no user: prefix)
	deniedUser := "mallory"      // parent-denied member of the broad group
	allowedUser := "bob"         // non-denied member of the broad group (control)
	interactPolicy := "group:" + group + "#member"

	// Parent authz: owner=alice; broad interact policy participant=group:<g>#member;
	// mallory AND bob are members of that group, but mallory is explicitly denied.
	touchRel(ctx, t, spdb, "group", group, "member", "user", deniedUser, "")
	touchRel(ctx, t, spdb, "group", group, "member", "user", allowedUser, "")
	require.NoError(t, spdb.TouchOwner(ctx, ns, parentName, "user:"+owner), "parent owner")
	require.NoError(t, spdb.TouchInteractParticipant(ctx, ns, parentName, interactPolicy), "parent interact policy")
	require.NoError(t, spdb.TouchDeniedUser(ctx, ns, parentName, identity.CanonicalFromTrusted(deniedUser, "test fixture")), "parent denied user")

	// Precondition: on the PARENT, the denied member has no interact (denied wins
	// over the group grant); the non-denied member does.
	require.False(t, checkPerm(ctx, t, spdb, "agentsession", ns+"/"+parentName, "interact", deniedUser),
		"precondition: parent-denied user must have no interact on the parent")
	require.True(t, checkPerm(ctx, t, spdb, "agentsession", ns+"/"+parentName, "interact", allowedUser),
		"precondition: a non-denied group member has interact on the parent")

	// Parent transcript so the inherit path carries something forward.
	mem := memory.NewLocal(inmem.NewBackend())
	parentScope := memory.Scope{Kind: "session", ID: ns + "/" + parentName}
	require.NoError(t, turn.NewAppender(mem, parentScope).Append(ctx, memory.Turn{
		Index: 0, Role: "user", CreatedAt: time.Unix(0, 0),
		Content: []memory.ContentBlock{{Type: "text", Text: "parent transcript turn"}},
	}))

	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: parentName, Namespace: ns,
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:" + owner},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:                     spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			AppliedInteractPermission: interactPolicy, // the broad policy carried onto the child
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				Mode:              spiceboxv1alpha1.PendingRestartModeInherit,
				NewUserText:       "please continue",
				TriggeredBy:       identity.Subject("user:" + owner), // the owner forks (fork = owner)
				TargetSessionName: childName,
			},
		},
	}
	armSignedRestart(t, parent)
	// restartStarterAllowed (start_gate.go) resolves the parent's class before
	// any fork mode proceeds; this class carries no allowlist, so the gate is
	// transparent and the leak assertion below is unaffected.
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(parent, ungatedClass(ns, "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     mem,
		AuthzGranter:      spdb,
		DeniedLister:      spdb,
		ForkChecker:       spdb,
		Snapshotter:       &recordingSnap{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	_, _, err = r.ReconcileRestart(ctx, parent)
	require.NoError(t, err, "inherit fork must complete")

	// Child materialized.
	var child spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: childName}, &child),
		"child session must exist after inherit fork")

	// THE SECURITY ASSERTION: the parent-denied user has NO interact on the CHILD.
	assert.False(t, checkPerm(ctx, t, spdb, "agentsession", ns+"/"+childName, "interact", deniedUser),
		"parent-denied user must NOT regain interact on the child (denied copied before the broad participant grant)")

	// Directly at the transcript layer: memory_entry#read = session->read_transcript,
	// so a memory_entry attached to the child resolves read=false for the denied user.
	//
	// memory_entry#session is claimed by pkg/memory/spicedbauthorizer in
	// production (see its Source var). This TOUCH only succeeds because
	// that package is not linked into THIS test binary today —
	// restartLeakTestSource above claims nothing, and the registry only
	// sees what a binary actually links (relsource.Register runs from
	// init()). If a future import pulls spicedbauthorizer in transitively,
	// this line starts failing with an ownership refusal — that is this
	// comment's whole purpose, so the cause is legible instead of a
	// mystery regression.
	entryID := uniqLeak(t, "entry-")
	touchRel(ctx, t, spdb, "memory_entry", entryID, "session", "agentsession", ns+"/"+childName, "")
	assert.False(t, checkPerm(ctx, t, spdb, "memory_entry", entryID, "read", deniedUser),
		"parent-denied user must be 403 on memory_entry#read of the child's inherited transcript")

	// Control: a non-denied member of the carried interact policy DOES gain
	// interact + read on the child — proving the fork itself works and the denial
	// is specific to the blocklisted user.
	assert.True(t, checkPerm(ctx, t, spdb, "agentsession", ns+"/"+childName, "interact", allowedUser),
		"a non-denied member of the carried interact policy gains interact on the child")
	assert.True(t, checkPerm(ctx, t, spdb, "memory_entry", entryID, "read", allowedUser),
		"a non-denied member can read the child's inherited transcript (control)")
}
