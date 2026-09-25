package agentsession_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lineage"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/tool_dispatch_snapshot"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
	"github.com/authzed/openagentprimitives/pkg/x/keyid"
)

func restartScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

// ungatedClass is the AgentClass fixture every ReconcileRestart test that
// isn't ABOUT the start gate now needs: restartStarterAllowed
// (start_gate.go) resolves the parent's class before any fork mode
// proceeds — takeover included — so a parent whose Spec.Class names nothing
// in the fake client now errors instead of silently forking. No
// spec.authz.session block, so StartGatePermission() is "" and the gate
// never asks a StartChecker (none of these fixtures wire one).
func ungatedClass(ns, name string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

// markerKeys is the test stand-in for channelsd's registered publisher key.
// Deterministic, so a signature computed in one test is reproducible in
// another, and package-level because ReconcileRestart now refuses any marker it
// cannot attribute to the connector.
type markerKeys map[[2]string]ed25519.PublicKey

func (k markerKeys) PublisherKey(publisher, keyID string) (ed25519.PublicKey, bool) {
	pub, ok := k[[2]string{publisher, keyID}]
	return pub, ok
}

// testMarkerSigner / testMarkerKeys are the package-wide connector identity for
// restart tests: one fixed keypair, the private half used to sign fixtures and
// the public half handed to the Reconciler as PublisherKeys.
//
// Every ReconcileRestart test needs both. status.pendingRestart is an
// authorization input the session's own runner can also write, so the
// reconciler verifies its author before acting (see pkg/agent/restartmarker) — a test
// whose fixture is unsigned exercises the refusal path, not the happy path.
var testMarkerSigner, testMarkerKeys = func() (*restartmarker.Signer, markerKeys) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 0x7a
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return restartmarker.NewSigner(priv, restartmarker.Publisher),
		markerKeys{{restartmarker.Publisher, keyid.For(pub)}: pub}
}()

// armSignedRestart stamps the connector attestation onto sess's PendingRestart,
// as channelsd does before writing it. Call it once the marker is fully built:
// the digest covers every field plus the parent's namespace, name and UID, so a
// later edit invalidates it.
func armSignedRestart(t *testing.T, sess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	require.NotNil(t, sess.Status.PendingRestart, "armSignedRestart needs a marker to sign")
	require.NoError(t, testMarkerSigner.Sign(sess, sess.Status.PendingRestart))
}

func TestEnsureChildSession_CreatesWhenMissing(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	c := fake.NewClientBuilder().WithScheme(restartScheme(t)).Build()
	parent := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}, Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo"}}
	pr := &spiceboxv1alpha1.PendingRestart{CutTurnIndex: 1, NewUserText: "x", TriggeredBy: "user:a", TargetSessionName: "p-fk1"}

	child, err := agentsession.EnsureChildSession(ctx, c, parent, pr)
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Equal(t, "p-fk1", child.Name)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p-fk1"}, &got))
	assert.Equal(t, "p", got.Spec.ForkedFrom)
}

func TestEnsureChildSession_AlreadyExists_ReturnsExisting(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	existing := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p-fk1", Namespace: "ns"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo", ForkedFrom: "p"},
	}
	c := fake.NewClientBuilder().WithScheme(restartScheme(t)).WithObjects(existing).Build()
	parent := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}, Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo"}}
	pr := &spiceboxv1alpha1.PendingRestart{CutTurnIndex: 1, NewUserText: "x", TriggeredBy: "user:a", TargetSessionName: "p-fk1"}

	child, err := agentsession.EnsureChildSession(ctx, c, parent, pr)
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Equal(t, "p-fk1", child.Name)

	var list spiceboxv1alpha1.AgentSessionList
	require.NoError(t, c.List(ctx, &list))
	assert.Len(t, list.Items, 1)
}

func TestSupersedeParent_SetsStatusFields(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseIdle},
	}
	c := fake.NewClientBuilder().WithScheme(restartScheme(t)).WithObjects(parent).WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()

	require.NoError(t, agentsession.SupersedeParent(ctx, c, parent, "p-fk1"))

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))
	assert.Equal(t, "p-fk1", got.Status.SupersededBy)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, got.Status.Phase)
	found := false
	for _, cond := range got.Status.Conditions {
		if cond.Type == spiceboxv1alpha1.AgentSessionConditionSupersededByRestart {
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			assert.Equal(t, spiceboxv1alpha1.ReasonReplacedByForkedSession, cond.Reason)
			found = true
		}
	}
	assert.True(t, found, "SupersededByRestart condition must be set")
}

func TestSupersedeParent_Idempotent(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseIdle},
	}
	c := fake.NewClientBuilder().WithScheme(restartScheme(t)).WithObjects(parent).WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()

	for i := 0; i < 3; i++ {
		var fresh spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &fresh))
		require.NoError(t, agentsession.SupersedeParent(ctx, c, &fresh, "p-fk1"))
	}
}

func TestCopyMemoryPrefix_CopiesTurnsAndWritesInbox(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	parent := memorypkg.Scope{Kind: "session", ID: "ns/p"}
	child := memorypkg.Scope{Kind: "session", ID: "ns/c"}

	appender := turn.NewAppender(mem, parent)
	for i := 0; i <= 3; i++ {
		require.NoError(t, appender.Append(ctx, memorypkg.Turn{
			Index: i, Role: "user", CreatedAt: time.Unix(int64(i), 0),
			Content: []memorypkg.ContentBlock{{Type: "text", Text: "u"}},
		}))
	}
	// A turn 4 we want skipped.
	require.NoError(t, appender.Append(ctx, memorypkg.Turn{
		Index: 4, Role: "user", CreatedAt: time.Unix(4, 0),
		Content: []memorypkg.ContentBlock{{Type: "text", Text: "skipped"}},
	}))

	require.NoError(t, agentsession.CopyMemoryPrefix(ctx, mem, parent, child, 3, "edited", plangate.ForkInherit))

	got, err := turn.NewAppender(mem, child).ReadAll(ctx)
	require.NoError(t, err)
	require.Len(t, got, 5, "turns 0..3 + inbox at 4")
	// The inbox turn is at index 4.
	var inboxIdx, inboxFound = 0, false
	for _, tn := range got {
		if tn.Role == "inbox" {
			inboxFound = true
			inboxIdx = tn.Index
			require.NotEmpty(t, tn.Content)
			assert.Equal(t, "edited", tn.Content[0].Text)
		}
	}
	assert.True(t, inboxFound, "inbox turn must be appended")
	assert.Equal(t, 4, inboxIdx)
}

func TestRecordRestartLineage_WritesBothDirections(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	parent := memorypkg.Scope{Kind: "session", ID: "ns/p"}
	child := memorypkg.Scope{Kind: "session", ID: "ns/c"}

	require.NoError(t, agentsession.RecordRestartLineage(ctx, mem, parent, "p", child, "c", 5, lineage.ReasonRestart))

	out, err := lineage.OutEdges(ctx, mem, parent)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "c", out[0].Peer)
	assert.Equal(t, 5, out[0].AtTurn)

	in, err := lineage.InEdges(ctx, mem, child)
	require.NoError(t, err)
	require.Len(t, in, 1)
	assert.Equal(t, "p", in[0].Peer)
}

type fakeGranter struct {
	startedBy     []string // "ns/name|subject"
	interactParts []string // "ns/name|subject"
	deniedUsers   []string // "ns/name|canonicalID"
	ownerCalls    int      // WriteSpiceDBParticipants must NOT write owner (child re-resolves its own)
	// touchOrder records the sequence of denied vs interact-participant touches
	// ("denied" / "interact") so tests can pin the security-critical ordering
	// (denied blocklist MUST land before the broad interact grant).
	touchOrder []string
}

func (f *fakeGranter) TouchStartedBy(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	f.startedBy = append(f.startedBy, ns+"/"+name+"|"+canonicalID.String())
	return nil
}
func (f *fakeGranter) TouchOwner(_ context.Context, _, _, _ string) error {
	f.ownerCalls++
	return nil
}
func (f *fakeGranter) TouchInteractParticipant(_ context.Context, ns, name, subject string) error {
	f.interactParts = append(f.interactParts, ns+"/"+name+"|"+subject)
	f.touchOrder = append(f.touchOrder, "interact")
	return nil
}
func (f *fakeGranter) TouchInteractParticipantUser(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	return nil
}
func (f *fakeGranter) TouchDeniedUser(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	f.deniedUsers = append(f.deniedUsers, ns+"/"+name+"|"+canonicalID.String())
	f.touchOrder = append(f.touchOrder, "denied")
	return nil
}
func (f *fakeGranter) DeleteStartedBy(_ context.Context, ns, name string) error { return nil }
func (f *fakeGranter) DeleteInteractParticipant(_ context.Context, ns, name, subject string) error {
	return nil
}
func (f *fakeGranter) TouchInteractor(_ context.Context, _, _, _ string) error { return nil }

// fakeDeniedLister returns a fixed denied-user list for the parent session so
// the fork reconciler can copy it onto the child. err (when set) surfaces a
// read failure the reconciler must not swallow.
type fakeDeniedLister struct {
	denied []string
	err    error
}

func (f fakeDeniedLister) ListDeniedUsers(_ context.Context, _, _ string) ([]string, error) {
	return f.denied, f.err
}

func TestWriteSpiceDBParticipants_CopiesParentSubjects(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	g := &fakeGranter{}
	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "ns",
			Annotations: map[string]string{
				// AnnotationStartedByCanonicalID stores "user:<canonical>" —
				// WriteSpiceDBParticipants must strip the prefix before calling
				// TouchStartedBy (which adds user: itself via the SpiceDB ObjectType).
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice",
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			// The snapshot is a subject SET: channelsd copies
			// AgentClass.spec.authz.session.interactPermission, which the class
			// controller regex-validates to "<type>:<id>#<relation>", and the
			// SpiceDB writer parses with ParseSubject (which requires the
			// #relation). A bare "user:<id>" — what this fixture used to carry —
			// is not a value either side accepts.
			AppliedInteractPermission: "group:eng#member",
		},
	}
	// The child carries its own started-by (BuildChildSession copies the
	// parent's for restart/inherit); WriteSpiceDBParticipants reads it there.
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
		Name: "p-fk", Namespace: "ns",
		Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
	}}

	require.NoError(t, agentsession.WriteSpiceDBParticipants(ctx, g, parent, child))

	// started_by: "user:" prefix must be stripped — TouchStartedBy adds it
	// via SpiceDB's ObjectType; passing "user:alice" would produce "user:user:alice".
	assert.Contains(t, g.startedBy, "ns/p-fk|alice")
	// interact: AppliedInteractPermission is a subject-set expression passed
	// verbatim to TouchInteractParticipant (no prefix stripping needed).
	assert.Contains(t, g.interactParts, "ns/p-fk|group:eng#member")
}

// TestWriteSpiceDBParticipants_CopiesParentInteractPolicyOntoChild is the
// coverage the takeover tests claim and did not have: whatever
// AppliedInteractPermission the PARENT status carries becomes an
// agentsession:<child>#participant grant, and `interact = owner + started_by +
// participant - denied` turns that into interact — hence memory_entry#read —
// on the child. On a takeover the child is owned by a DIFFERENT human, so this
// slice is where a forged parent value lands. It is asserted here (rather than
// only in the envtest suite) because the vacuous assertion this replaces read
// g.startedBy, which cannot hold an interact grant.
func TestWriteSpiceDBParticipants_CopiesParentInteractPolicyOntoChild(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	g := &fakeGranter{}
	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "ns",
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{AppliedInteractPermission: "group:attackers#member"},
	}
	// Takeover: the child is owned by bob, not by the parent's alice.
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
		Name: "p-tk", Namespace: "ns",
		Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:bob"},
	}}

	require.NoError(t, agentsession.WriteSpiceDBParticipants(ctx, g, parent, child))

	assert.Equal(t, []string{"ns/p-tk|group:attackers#member"}, g.interactParts,
		"the parent's snapshotted policy is granted participant on the CHILD verbatim — which is why "+
			"status.appliedInteractPermission must not be writable by a session runner (webhook pin)")
	assert.Equal(t, []string{"ns/p-tk|bob"}, g.startedBy, "started_by is the child's own owner")
}

// TestWriteSpiceDBParticipants_RejectsMalformedInteractPermission pins the
// defense-in-depth half of the same gate: the snapshot is validated before it
// becomes a #participant grant, exactly as the sibling started_by subject is
// (ValidateSubject, above it in the same function). Every value here is one
// the legitimate writer cannot produce — channelsd copies
// AgentClass.spec.authz.session.interactPermission, which the AgentClass
// controller regex-validates to "<type>:<id>#<relation>".
func TestWriteSpiceDBParticipants_RejectsMalformedInteractPermission(t *testing.T) {
	cases := []struct {
		name string
		perm string
	}{
		{
			name: "concrete user with no #relation: rejected (spicedb.ParseSubject requires a subject set, so this reaches SpiceDB as an error)",
			perm: "user:alice",
		},
		{name: "not a subject at all: rejected", perm: "notasubject"},
		{name: "empty relation: rejected", perm: "group:eng#"},
		{
			// The admissible types are derived from the composed schema — the
			// scaffold's own vocabulary plus every registered kind's session
			// links. agentsession is in neither, so it stays refused.
			name: "a type the composed schema does not admit as participant: rejected",
			perm: "agentsession:ns/other#owner",
		},
		{name: "whitespace in the object id: rejected", perm: "group:eng team#member"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memorypkg.WithSystemApproval(context.Background(), "test")
			g := &fakeGranter{}
			parent := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{
					Name: "p", Namespace: "ns",
					Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
				},
				Status: spiceboxv1alpha1.AgentSessionStatus{AppliedInteractPermission: tc.perm},
			}
			child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
				Name: "p-tk", Namespace: "ns",
				Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:bob"},
			}}

			err := agentsession.WriteSpiceDBParticipants(ctx, g, parent, child)
			require.Error(t, err, "interact permission %q must abort the fork", tc.perm)
			assert.Empty(t, g.interactParts, "no participant write on a rejected subject")
		})
	}
}

// TestWriteSpiceDBParticipants_StripsUserPrefixFromStartedBy is an explicit
// regression test for the "user:user:<canonical>" double-prefix bug: the
// annotation stores "user:<canonical>" but TouchStartedBy's SpiceDB call
// already wraps the value in ObjectType "user", so we must strip the prefix.
func TestWriteSpiceDBParticipants_StripsUserPrefixFromStartedBy(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	g := &fakeGranter{}
	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "ns",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:am9leUBhdXRoemVkLmNvbQ",
			},
		},
	}
	// Child carries the same started-by (BuildChildSession copied it forward).
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
		Name: "p-fk", Namespace: "ns",
		Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:am9leUBhdXRoemVkLmNvbQ"},
	}}

	require.NoError(t, agentsession.WriteSpiceDBParticipants(ctx, g, parent, child))

	// Must receive the raw canonical ("am9leUBhdXRoemVkLmNvbQ"), NOT "user:am9leUBhdXRoemVkLmNvbQ".
	require.Len(t, g.startedBy, 1)
	assert.Equal(t, "ns/p-fk|am9leUBhdXRoemVkLmNvbQ", g.startedBy[0])
}

// TestWriteSpiceDBParticipants_DoesNotWriteOwner pins the ownership boundary:
// the fork copies started_by + interact, but NOT owner — the child re-resolves
// and re-mints its OWN owner from its carried started-by annotation during its
// normal reconcile (so a copied owner tuple can't outlive a policy change).
func TestWriteSpiceDBParticipants_DoesNotWriteOwner(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	g := &fakeGranter{}
	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "ns",
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{AppliedInteractPermission: "group:eng#member"},
	}
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
		Name: "p-fk", Namespace: "ns",
		Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
	}}

	require.NoError(t, agentsession.WriteSpiceDBParticipants(ctx, g, parent, child))
	assert.Zero(t, g.ownerCalls, "fork must not copy owner; the child re-resolves its own")
}

// TestWriteSpiceDBParticipants_RejectsMalformedStartedBy verifies the fork
// validates the started_by subject before stamping it (ValidateSubject runs
// before started_by/owner is written). A subject-set or non-user type aborts the fork.
func TestWriteSpiceDBParticipants_RejectsMalformedStartedBy(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	for _, bad := range []string{"group:eng#member", "user:bad:colon", "notasubject"} {
		g := &fakeGranter{}
		parent := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p", Namespace: "ns",
				Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: bad},
			},
		}
		child := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p-fk", Namespace: "ns",
				Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: bad},
			},
		}
		err := agentsession.WriteSpiceDBParticipants(ctx, g, parent, child)
		require.Error(t, err, "started_by %q must be rejected", bad)
		assert.Empty(t, g.startedBy, "no started_by write on a rejected subject")
	}
}

// TestWriteSpiceDBParticipants_TakeoverGrantsNewOwner pins the takeover
// ownership transfer: the child's started_by (which feeds
// interact = (started_by + participant) - denied) is the NEW owner (bob), read
// from the CHILD's own annotation — NOT the parent's owner (alice). If the
// prior owner's subject leaked into the child's started_by, alice would retain
// interact (hence view) over bob's new session.
func TestWriteSpiceDBParticipants_TakeoverGrantsNewOwner(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	g := &fakeGranter{}
	parent := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "ns",
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
		},
	}
	child := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p-tk", Namespace: "ns",
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:bob"},
		},
	}

	require.NoError(t, agentsession.WriteSpiceDBParticipants(ctx, g, parent, child))

	assert.Contains(t, g.startedBy, "ns/p-tk|bob", "child owner = bob (the taking-over user)")
	assert.NotContains(t, g.startedBy, "ns/p-tk|alice", "child started_by is read from the CHILD's annotation, never the parent's")
	// interact has a second source — participant — and it lands in a DIFFERENT
	// slice. Asserting only on startedBy claimed a guarantee it could not
	// observe: this parent carries no interact policy, so no participant may be
	// granted at all. See _CopiesParentInteractPolicyOntoChild for the case
	// where it does.
	assert.Empty(t, g.interactParts, "a parent with no interact policy grants no participant on the child")
}

// TestCopyMemoryPrefix_NegativeCutSeedsOnlyNewMessage pins the policy-halt
// takeover carve-out: cut = -1 copies NO prior turns (the halted transcript is
// not handed to the new owner), and the new user's message becomes turn 0.
func TestCopyMemoryPrefix_NegativeCutSeedsOnlyNewMessage(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	src := memorypkg.Scope{Kind: "session", ID: "ns/p"}
	dst := memorypkg.Scope{Kind: "session", ID: "ns/c"}

	appender := turn.NewAppender(mem, src)
	for i := 0; i <= 2; i++ {
		require.NoError(t, appender.Append(ctx, memorypkg.Turn{
			Index: i, Role: "user", CreatedAt: time.Unix(int64(i), 0),
			Content: []memorypkg.ContentBlock{{Type: "text", Text: "secret-history"}},
		}))
	}

	require.NoError(t, agentsession.CopyMemoryPrefix(ctx, mem, src, dst, -1, "bobs fresh message", plangate.ForkInherit))

	childTurns, err := turn.ReadAll(ctx, mem, dst)
	require.NoError(t, err)
	require.Len(t, childTurns, 1, "fresh takeover seeds only the new user's message")
	assert.Equal(t, 0, childTurns[0].Index)
	assert.Equal(t, "inbox", childTurns[0].Role)
	for _, ct := range childTurns {
		for _, cb := range ct.Content {
			assert.NotContains(t, cb.Text, "secret-history", "no prior transcript may leak into a fresh takeover")
		}
	}
}

// TestReconcileRestart_DeletedClassDeniesRatherThanWedging pins the one class
// read failure that must NOT requeue.
//
// ReconcileRestart runs above chain-head anchoring and the terminal reap, and
// it short-circuits the rest of Reconcile whenever a marker is present. So a
// terminal session still carrying a pending restart whose agent was deleted
// afterwards — the ordinary shape: someone deletes an agent, a person in the
// thread asks to continue one of its finished sessions — would error on every
// pass forever, and the session would never be anchored or reaped. Nothing
// about it self-heals: a deleted class does not come back.
//
// So a NotFound DENIES, burning the marker, and every other read failure stays
// a hard error (that one really is transient).
func TestReconcileRestart_DeletedClassDeniesRatherThanWedging(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "ns",
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "deleted-agent"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				Mode:              spiceboxv1alpha1.PendingRestartModeTakeover,
				NewUserText:       "taking this over",
				TriggeredBy:       "user:bob",
				TargetSessionName: "p-tk1",
			},
		},
	}
	armSignedRestart(t, sess)

	// No AgentClass object at all: the agent was deleted after the session
	// finished.
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     memorypkg.NewLocal(inmem.NewBackend()),
		AuthzGranter:      &fakeGranter{},
		Snapshotter:       &fakeSnapshotter{},
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	proceed, _, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err, "a deleted class is a definitive denial, not a retryable failure")
	assert.False(t, proceed)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))
	assert.Nil(t, got.Status.PendingRestart,
		"the marker must burn, or anchoring and the reap never run")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRestartDenied)
	require.NotNil(t, cond, "the person who asked must be told, via the same relay every other denial uses")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentClassMissing, cond.Reason,
		"a missing agent is its own reason; reusing NotAnAllowedStarter would send an operator to debug a list that no longer exists")
	assert.Contains(t, cond.Message, "no longer exists")
	for _, internal := range []string{"AgentClass", "NotFound", "kubectl", "namespace"} {
		assert.NotContains(t, cond.Message, internal, "no internal vocabulary in what the person reads")
	}

	// Nothing was materialized on the way to the denial.
	childErr := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p-tk1"}, &spiceboxv1alpha1.AgentSession{})
	assert.True(t, apierrors.IsNotFound(childErr), "a denied takeover must create no child AgentSession")
}

// TestReconcileRestart_InheritCopiesFullTranscriptAndDenied is the I4 leak
// test: a continuation-inherit fork copies the WHOLE parent transcript, copies
// the parent's denied blocklist onto the child (so a parent-denied user has no
// interact — hence no view — over the inherited history), records a
// continuation lineage edge, creates the child, and supersedes the parent.
func TestReconcileRestart_InheritCopiesFullTranscriptAndDenied(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	parentScope := memorypkg.Scope{Kind: "session", ID: "ns/p"}

	// Seed a full parent transcript (turns 0..2).
	appender := turn.NewAppender(mem, parentScope)
	for i := 0; i <= 2; i++ {
		require.NoError(t, appender.Append(ctx, memorypkg.Turn{
			Index: i, Role: "user", CreatedAt: time.Unix(int64(i), 0),
			Content: []memorypkg.ContentBlock{{Type: "text", Text: "t"}},
		}))
	}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "ns",
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				Mode:              spiceboxv1alpha1.PendingRestartModeInherit,
				NewUserText:       "please continue",
				TriggeredBy:       "user:alice",
				TargetSessionName: "p-ic1",
			},
		},
	}
	armSignedRestart(t, sess)
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess, ungatedClass("ns", "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	g := &fakeGranter{}
	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     mem,
		AuthzGranter:      g,
		Snapshotter:       &fakeSnapshotter{}, // no error → CLEAN clone succeeds
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{denied: []string{"mallory"}},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	proceed, _, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed)

	// I4: the parent's denied user is denied on the child.
	assert.Contains(t, g.deniedUsers, "ns/p-ic1|mallory",
		"parent-denied user must be denied on the child (no regained interact over inherited history)")

	// Full transcript carried forward (turns 0..2) plus the inbox turn at 3.
	childScope := memorypkg.Scope{Kind: "session", ID: "ns/p-ic1"}
	childTurns, err := turn.ReadAll(ctx, mem, childScope)
	require.NoError(t, err)
	var inboxFound bool
	maxIdx := -1
	for _, tn := range childTurns {
		if tn.Index > maxIdx {
			maxIdx = tn.Index
		}
		if tn.Role == "inbox" {
			inboxFound = true
			assert.Equal(t, 3, tn.Index, "inbox lands contiguously after the last parent turn (2)")
		}
	}
	assert.True(t, inboxFound, "the new message becomes the child's inbox turn")
	assert.Equal(t, 3, maxIdx, "child holds turns 0..2 + inbox at 3 (full transcript)")

	// Lineage recorded as a continuation (not a restart cut).
	in, err := lineage.InEdges(ctx, mem, childScope)
	require.NoError(t, err)
	require.Len(t, in, 1)
	assert.Equal(t, lineage.ReasonContinuation, in[0].Reason, "inherit records a continuation lineage edge")

	// Child created; parent superseded.
	var child spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p-ic1"}, &child))
	assert.Equal(t, "p", child.Spec.ForkedFrom)
	var gotParent spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &gotParent))
	assert.Equal(t, "p-ic1", gotParent.Status.SupersededBy)
}

// TestReconcileRestart_ReplayAfterChildExists_StillSupersedesAndClears pins the
// resumability invariant: once the child CR exists, a retried ReconcileRestart
// MUST still reach SupersedeParent + clearPendingRestart.
//
// Seeding (CopyMemoryPrefix / lineage) deliberately runs BEFORE the child CR is
// created, so "child exists" already implies "seeding completed". Replaying the
// seed on a retry instead writes into a scope the child's runner now owns, and
// the inbox append collides:
//
//	ReconcileRestart: copy memory: CopyMemoryPrefix: append inbox: memory: index conflict
//
// Because that failure precedes Steps 8-9, the parent is never superseded and
// PendingRestart becomes immortal. A stale marker makes channelsd's
// writeInheritForkTrigger short-circuit forever, so every later reply is
// acknowledged with "I'm picking it up in a new session" and the session never arrives.
func TestReconcileRestart_ReplayAfterChildExists_StillSupersedesAndClears(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	parentScope := memorypkg.Scope{Kind: "session", ID: "ns/p"}
	childScope := memorypkg.Scope{Kind: "session", ID: "ns/p-ic1"}

	// Parent transcript, turns 0..2.
	pa := turn.NewAppender(mem, parentScope)
	for i := 0; i <= 2; i++ {
		require.NoError(t, pa.Append(ctx, memorypkg.Turn{
			Index: i, Role: "user", CreatedAt: time.Unix(int64(i), 0),
			Content: []memorypkg.ContentBlock{{Type: "text", Text: "t"}},
		}))
	}

	// A first attempt already seeded the child scope (turns 0..2 + inbox at 3)
	// and created the child CR. Its runner has since advanced the scope: an
	// inbox turn at 4 carrying different content than the marker's NewUserText.
	ca := turn.NewAppender(mem, childScope)
	for i := 0; i <= 2; i++ {
		require.NoError(t, ca.Append(ctx, memorypkg.Turn{
			Index: i, Role: "user", CreatedAt: time.Unix(int64(i), 0),
			Content: []memorypkg.ContentBlock{{Type: "text", Text: "t"}},
		}))
	}
	require.NoError(t, ca.Append(ctx, memorypkg.Turn{
		Index: 3, Role: "inbox", CreatedAt: time.Unix(3, 0),
		Content: []memorypkg.ContentBlock{{Type: "text", Text: "please continue"}},
	}))
	require.NoError(t, ca.Append(ctx, memorypkg.Turn{
		Index: 4, Role: "inbox", CreatedAt: time.Unix(4, 0),
		Content: []memorypkg.ContentBlock{{Type: "text", Text: "a later reply from the user"}},
	}))

	// The parent's scope drifted forward too, so the recomputed effective cut is
	// now 3 and the replayed seed targets inbox@4 — which the child already owns
	// with different content.
	require.NoError(t, pa.Append(ctx, memorypkg.Turn{
		Index: 3, Role: "user", CreatedAt: time.Unix(3, 0),
		Content: []memorypkg.ContentBlock{{Type: "text", Text: "drift"}},
	}))

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "ns",
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				Mode:              spiceboxv1alpha1.PendingRestartModeInherit,
				NewUserText:       "please continue",
				TriggeredBy:       "user:alice",
				TargetSessionName: "p-ic1",
			},
		},
	}
	child := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p-ic1", Namespace: "ns"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{ForkedFrom: "p"},
	}
	armSignedRestart(t, sess)
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess, child, ungatedClass("ns", "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     mem,
		AuthzGranter:      &fakeGranter{},
		Snapshotter:       &fakeSnapshotter{},
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	_, _, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err, "a retry after the child exists must not wedge on a replayed memory seed")

	var gotParent spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &gotParent))
	assert.Equal(t, "p-ic1", gotParent.Status.SupersededBy, "parent must be superseded on the retry")
	assert.Nil(t, gotParent.Status.PendingRestart, "the marker must be cleared, not left immortal")
}

// TestReconcileRestart_DeniedCopiedBeforeInteractGrant pins the security-
// critical ordering in the inherit fork: the parent's denied blocklist MUST be
// copied onto the child BEFORE the broad interact participant grant lands. With
// a group/org interact policy, granting participant first opens a window where
// interact (= owner + participant − denied) evaluates against an EMPTY denied
// set, briefly re-granting a parent-denied member read over the child's
// inherited transcript. Copying denied first closes that window.
func TestReconcileRestart_DeniedCopiedBeforeInteractGrant(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	parentScope := memorypkg.Scope{Kind: "session", ID: "ns/p"}

	// A parent transcript so the inherit path has something to carry forward.
	require.NoError(t, turn.NewAppender(mem, parentScope).Append(ctx, memorypkg.Turn{
		Index: 0, Role: "user", CreatedAt: time.Unix(0, 0),
		Content: []memorypkg.ContentBlock{{Type: "text", Text: "t"}},
	}))

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "ns",
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			// A broad interact policy — the exact case where an empty-denied
			// window would leak: interact resolves through a group subject-set.
			AppliedInteractPermission: "group:eng#member",
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				Mode:              spiceboxv1alpha1.PendingRestartModeInherit,
				NewUserText:       "please continue",
				TriggeredBy:       "user:alice",
				TargetSessionName: "p-ic1",
			},
		},
	}
	armSignedRestart(t, sess)
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess, ungatedClass("ns", "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	g := &fakeGranter{}
	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     mem,
		AuthzGranter:      g,
		Snapshotter:       &fakeSnapshotter{},
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{denied: []string{"mallory"}},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	proceed, _, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed)

	// The denied blocklist landed strictly before the broad interact grant, so
	// interact is never evaluated against an empty denied set.
	assert.Equal(t, []string{"denied", "interact"}, g.touchOrder,
		"denied must be copied BEFORE the interact participant grant (no empty-denied window)")
	assert.Contains(t, g.deniedUsers, "ns/p-ic1|mallory", "parent-denied user denied on the child")
	assert.Contains(t, g.interactParts, "ns/p-ic1|group:eng#member", "broad interact policy carried forward")
}

// TestReconcileRestart_InheritForkDenied_NonOwner is I3 for the inherit path:
// the fork gate rejects a non-owner forker exactly as it does for restart —
// no child, no memory copy, no denied copy, PendingRestart cleared.
func TestReconcileRestart_InheritForkDenied_NonOwner(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				Mode:              spiceboxv1alpha1.PendingRestartModeInherit,
				NewUserText:       "continue",
				TriggeredBy:       "user:coworker",
				TargetSessionName: "p-ic1",
			},
		},
	}
	armSignedRestart(t, sess)
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess, ungatedClass("ns", "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	g := &fakeGranter{}
	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     mem,
		AuthzGranter:      g,
		Snapshotter:       &fakeSnapshotter{},
		ForkChecker:       fakeForkChecker{allow: false}, // non-owner
		DeniedLister:      fakeDeniedLister{denied: []string{"mallory"}},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	proceed, _, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed)

	var childGetErr = c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p-ic1"}, &spiceboxv1alpha1.AgentSession{})
	assert.True(t, apierrors.IsNotFound(childGetErr), "denied inherit must create no child")
	assert.Empty(t, g.deniedUsers, "denied fork must not copy the blocklist onto a child")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))
	assert.Nil(t, got.Status.PendingRestart, "PendingRestart cleared on deny (no retry loop)")
	assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRestartDenied))
}

// TestReconcileRestart_MissingDeniedLister_ReturnsError verifies the denied
// lister is a MANDATORY dependency: without it the fork errors fail-closed
// rather than materialize a child with an uncopied (weaker) blocklist.
func TestReconcileRestart_MissingDeniedLister_ReturnsError(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:          spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{Mode: spiceboxv1alpha1.PendingRestartModeInherit, NewUserText: "x", TriggeredBy: "user:a", TargetSessionName: "p-ic1"},
		},
	}
	// All deps except DeniedLister.
	r := &agentsession.Reconciler{
		Client:        fake.NewClientBuilder().WithScheme(restartScheme(t)).WithObjects(sess).WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build(),
		RestartMemory: mem,
		AuthzGranter:  &fakeGranter{},
		Snapshotter:   &fakeSnapshotter{},
		ForkChecker:   fakeForkChecker{allow: true},
	}
	proceed, _, err := r.ReconcileRestart(ctx, sess)
	require.Error(t, err)
	assert.False(t, proceed)
	assert.Contains(t, err.Error(), "DeniedLister")
}

func TestWriteSpiceDBParticipants_MissingAnnotations_Skip(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	g := &fakeGranter{}
	parent := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	child := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "p-fk", Namespace: "ns"}}

	require.NoError(t, agentsession.WriteSpiceDBParticipants(ctx, g, parent, child))

	// No started-by or interact subject on parent → no writes.
	assert.Empty(t, g.startedBy)
	assert.Empty(t, g.interactParts)
	_ = authz.SessionRef{}
}

// fakeForkChecker is the local authz.ForkChecker fake for the restart reconcile
// gate tests (the fake in pkg/authz tests lives in a different package).
type fakeForkChecker struct {
	allow bool
	err   error
}

func (f fakeForkChecker) CheckFork(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	return f.allow, f.err
}

// TestReconcileRestart_ForkDenied_AbortsNoChildSetsConditionNotifies verifies the
// SessionFork gate's fail-closed abort path: a non-owner forker creates NO child,
// copies NO memory, sets RestartDenied, delivers a requester notice, and clears
// PendingRestart (no reconcile loop).
func TestReconcileRestart_ForkDenied_AbortsNoChildSetsConditionNotifies(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseIdle,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				CutTurnIndex:      1,
				NewUserText:       "edited",
				TriggeredBy:       "user:bob",
				TargetSessionName: "p-fk1",
			},
		},
	}
	armSignedRestart(t, sess)
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess, ungatedClass("ns", "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	var noticeBody string
	noticeCalls := 0
	r := &agentsession.Reconciler{
		Client:        c,
		RestartMemory: mem,
		AuthzGranter:  &fakeGranter{},
		Snapshotter:   &fakeSnapshotter{}, // non-nil; never reached on deny
		ForkChecker:   fakeForkChecker{allow: false},
		DeniedLister:  fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, body string) error {
			noticeCalls++
			noticeBody = body
			return nil
		},
		PublisherKeys: testMarkerKeys,
	}

	proceed, _, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed, "denied fork must stop the outer reconcile")

	// No child created.
	var child spiceboxv1alpha1.AgentSession
	gerr := c.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Status.PendingRestart.TargetSessionName}, &child)
	assert.True(t, apierrors.IsNotFound(gerr), "no child AgentSession may be created on deny")

	// Parent: PendingRestart cleared + RestartDenied condition set.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, &got))
	assert.Nil(t, got.Status.PendingRestart, "PendingRestart must be cleared (no retry loop)")
	assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRestartDenied))

	// Notice delivered to the forker.
	assert.Equal(t, 1, noticeCalls)
	assert.NotEmpty(t, noticeBody)

	// The DURABLE condition carries the same requester-facing wording. This is
	// what actually reaches the user: channelsd's session watcher relays the
	// condition Message to the thread, because the out.metaagent_notice publish
	// asserted above is best-effort raw JSON that the wildcard outbound relay
	// also consumes and rejects ("unsupported envelope version 0"). A denial
	// recorded with an empty Message is a denial the user never sees.
	denied := meta.FindStatusCondition(got.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionRestartDenied)
	require.NotNil(t, denied, "RestartDenied condition must exist")
	assert.Equal(t, noticeBody, denied.Message,
		"condition Message must carry the requester-facing notice text")
}

// TestReconcileRestart_ForkAllowed_Proceeds verifies an allowing checker leaves
// the gate transparent: the reconcile proceeds into the existing materialization
// path (reuses the snapshot-missing fixture so it terminates deterministically)
// and does NOT set RestartDenied.
func TestReconcileRestart_ForkAllowed_Proceeds(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	parentScope := memorypkg.Scope{Kind: "session", ID: "ns/p"}

	// Seed a post-cut tool dispatch audit entry so AnalyzePostCut returns a
	// non-empty snapshots map (IMPACTFUL path); the snapshotter then returns
	// ErrSnapshotNotFound, ending the path at the snapshot-failed branch.
	require.NoError(t, tool_dispatch_snapshot.Record(ctx, mem, parentScope, tool_dispatch_snapshot.Content{
		ToolUseID: "u1", SpiceboxSession: "b1", TurnIndex: 3, Sequence: 0, SessionUID: "uid",
	}))

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseIdle,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				CutTurnIndex:      1,
				NewUserText:       "edited",
				TriggeredBy:       "user:alice",
				TargetSessionName: "p-fk1",
			},
		},
	}
	armSignedRestart(t, sess)
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess, ungatedClass("ns", "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	snap := &fakeSnapshotter{restoreErr: workspace.ErrSnapshotNotFound}
	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     mem,
		AuthzGranter:      &fakeGranter{},
		Snapshotter:       snap,
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}
	proceed, _, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, &got))
	assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRestartDenied),
		"an allowed fork must not set RestartDenied")
}

func TestReconcileRestart_NoPendingRestart_ProceedTrue(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
	}
	// No deps wired — should still return proceed=true without error.
	r := &agentsession.Reconciler{}
	proceed, res, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err)
	assert.True(t, proceed)
	assert.Equal(t, ctrl.Result{}, res)
}

func TestReconcileRestart_MissingDeps_ReturnsError(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:          spiceboxv1alpha1.AgentSessionPhaseIdle,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{CutTurnIndex: 1, NewUserText: "x", TriggeredBy: "user:a", TargetSessionName: "s-fk1"},
		},
	}
	// No deps wired — should fail with a descriptive error.
	r := &agentsession.Reconciler{}
	proceed, _, err := r.ReconcileRestart(ctx, sess)
	require.Error(t, err)
	assert.False(t, proceed)
	assert.Contains(t, err.Error(), "missing dependency")
}

// TestReconcileRestart_SnapshotMissing_SetsConditionAndClearsPending
// verifies that when RestoreOrCloneBundlePVCs returns
// workspace.ErrSnapshotNotFound the reconciler marks the parent
// WorkspaceSnapshotFailed=True, clears PendingRestart, and returns
// proceed=false without error so the outer reconcile exits cleanly.
func TestReconcileRestart_SnapshotMissing_SetsConditionAndClearsPending(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	parentScope := memorypkg.Scope{Kind: "session", ID: "ns/p"}

	// Seed a post-cut tool dispatch audit entry so AnalyzePostCut
	// returns a non-empty snapshots map (IMPACTFUL path). The
	// fakeSnapshotter's Restore will then return ErrSnapshotNotFound.
	require.NoError(t, tool_dispatch_snapshot.Record(ctx, mem, parentScope, tool_dispatch_snapshot.Content{
		ToolUseID: "u1", SpiceboxSession: "b1", TurnIndex: 3, Sequence: 0, SessionUID: "uid",
	}))

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseIdle,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				CutTurnIndex:      1,
				NewUserText:       "edited",
				TriggeredBy:       "user:alice",
				TargetSessionName: "p-fk1",
			},
		},
	}
	armSignedRestart(t, sess)
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess, ungatedClass("ns", "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	snap := &fakeSnapshotter{restoreErr: workspace.ErrSnapshotNotFound}
	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     mem,
		AuthzGranter:      &fakeGranter{},
		Snapshotter:       snap,
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	proceed, res, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err, "ErrSnapshotNotFound must be handled, not propagated")
	assert.False(t, proceed)
	assert.Equal(t, ctrl.Result{}, res)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))
	assert.Nil(t, got.Status.PendingRestart, "PendingRestart must be cleared")

	found := false
	for _, cond := range got.Status.Conditions {
		if cond.Type == spiceboxv1alpha1.AgentSessionConditionWorkspaceSnapshotFailed {
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			assert.Equal(t, spiceboxv1alpha1.ReasonSnapshotJobFailed, cond.Reason)
			found = true
		}
	}
	assert.True(t, found, "WorkspaceSnapshotFailed=True condition must be set")
}

// TestReconcileRestart_PendingRestore_RequeuesAndPreservesMarker pins the
// wait-for-completion hold: while the CLEAN-fork snapshot Job is still
// running, ReconcileRestart must requeue at the poll interval WITHOUT
// superseding the parent and WITHOUT clearing PendingRestart — this is the
// one legitimate in-progress hold, because SupersedeParent must not run
// until the child's workspace is actually populated.
func TestReconcileRestart_PendingRestore_RequeuesAndPreservesMarker(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseIdle,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				CutTurnIndex:      1,
				NewUserText:       "edited",
				TriggeredBy:       "user:alice",
				TargetSessionName: "p-fk1",
			},
		},
	}
	armSignedRestart(t, sess)
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess, ungatedClass("ns", "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	// No post-cut dispatches seeded → CLEAN path; the ad-hoc snapshot Job
	// is created but has not completed yet.
	snap := &fakeSnapshotter{snapshotPending: true}
	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     mem,
		AuthzGranter:      &fakeGranter{},
		Snapshotter:       snap,
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	proceed, res, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err, "a pending restore is not an error")
	assert.False(t, proceed)
	assert.Equal(t, 5*time.Second, res.RequeueAfter,
		"pending restore must requeue at restorePollInterval")
	assert.Empty(t, snap.restoreCalls, "no restore Job before the snapshot completes")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))
	assert.NotNil(t, got.Status.PendingRestart,
		"PendingRestart must be PRESERVED while the copy is in flight")
	assert.Empty(t, got.Status.SupersededBy,
		"the parent must not be superseded before the restore completes")

	// Once the Jobs complete, the requeued reconcile converges: restore
	// runs, the parent is superseded, and the marker clears.
	snap.snapshotPending = false
	proceed, _, err = r.ReconcileRestart(ctx, &got)
	require.NoError(t, err)
	assert.False(t, proceed)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))
	assert.Equal(t, "p-fk1", got.Status.SupersededBy, "requeued reconcile completes the fork")
	assert.Nil(t, got.Status.PendingRestart)
	require.Len(t, snap.restoreCalls, 1, "restore launched once the snapshot completed")
}

// TestReconcileRestart_SnapshotJobFailed_SetsConditionAndClearsPending pins
// terminal-Job-failure classification: a snapshot/restore Job that exhausted
// its backoff (SnapshotDone → ErrSnapshotJobFailed) is permanent — the parent
// gets WorkspaceSnapshotFailed=True and PendingRestart is cleared, exactly
// like a missing snapshot or a 4xx apiserver rejection. Before this
// classification the failure was invisible: nothing observed the failed Job
// and the child silently started with an empty/partial workspace.
func TestReconcileRestart_SnapshotJobFailed_SetsConditionAndClearsPending(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseIdle,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				CutTurnIndex:      1,
				NewUserText:       "edited",
				TriggeredBy:       "user:alice",
				TargetSessionName: "p-fk1",
			},
		},
	}
	armSignedRestart(t, sess)
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess, ungatedClass("ns", "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	// CLEAN path; the ad-hoc snapshot Job failed terminally.
	snap := &fakeSnapshotter{snapshotDoneErr: fmt.Errorf(
		"CPByPod: Job ns/snap-x failed: BackoffLimitExceeded: %w", workspace.ErrSnapshotJobFailed)}
	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     mem,
		AuthzGranter:      &fakeGranter{},
		Snapshotter:       snap,
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	proceed, res, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err, "a terminal Job failure must be handled, not propagated")
	assert.False(t, proceed)
	assert.Equal(t, ctrl.Result{}, res)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))
	assert.Nil(t, got.Status.PendingRestart, "PendingRestart must be cleared — never immortal")

	found := false
	for _, cond := range got.Status.Conditions {
		if cond.Type == spiceboxv1alpha1.AgentSessionConditionWorkspaceSnapshotFailed {
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			assert.Equal(t, spiceboxv1alpha1.ReasonSnapshotJobFailed, cond.Reason)
			found = true
		}
	}
	assert.True(t, found, "WorkspaceSnapshotFailed=True condition must be set")
}

// TestReconcileRestart_PermanentSnapshotError_ClearsPending is the regression
// test for the zap2 immortal-PendingRestart wedge: the CLEAN-fork snapshot Job
// was rejected by the apiserver (invalid turnIndex label), the error was not
// ErrSnapshotNotFound, and ReconcileRestart retried it forever — the parent
// kept an immortal PendingRestart and the fork never completed. A permanent
// (4xx) snapshot error must take the same condition+clear path as a missing
// snapshot.
func TestReconcileRestart_PermanentSnapshotError_ClearsPending(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseIdle,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				CutTurnIndex:      1,
				NewUserText:       "edited",
				TriggeredBy:       "user:alice",
				TargetSessionName: "p-fk1",
			},
		},
	}
	armSignedRestart(t, sess)
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess, ungatedClass("ns", "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	// No post-cut dispatches seeded → CLEAN path → Snapshot is called and
	// returns a permanent apiserver rejection.
	snap := &fakeSnapshotter{snapshotErr: apierrors.NewInvalid(
		schema.GroupKind{Group: "batch", Kind: "Job"}, "snap-x",
		field.ErrorList{field.Invalid(field.NewPath("metadata", "labels"), "-1", "invalid label value")})}
	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     mem,
		AuthzGranter:      &fakeGranter{},
		Snapshotter:       snap,
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	proceed, res, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err, "a permanent snapshot error must be handled, not propagated")
	assert.False(t, proceed)
	assert.Equal(t, ctrl.Result{}, res)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))
	assert.Nil(t, got.Status.PendingRestart, "PendingRestart must be cleared — never immortal")

	found := false
	for _, cond := range got.Status.Conditions {
		if cond.Type == spiceboxv1alpha1.AgentSessionConditionWorkspaceSnapshotFailed {
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			assert.Equal(t, spiceboxv1alpha1.ReasonSnapshotJobFailed, cond.Reason)
			found = true
		}
	}
	assert.True(t, found, "WorkspaceSnapshotFailed=True condition must be set")
}

// TestReconcileRestart_TakeoverMarkerAuthorIsVerified is the privilege-escalation
// regression test for the fork/takeover marker.
//
// The runner Role grants patch on agentsessions/status (rbac.go), and Kubernetes
// RBAC has no field granularity, so a compromised runner can write
// status.pendingRestart on its OWN session. Takeover mode deliberately skips the
// SpiceDB agentsession#fork gate — correctly, since the parent's #fork relation
// cannot hold for a different user — and BuildChildSession then stamps
// pendingRestart.triggeredBy onto the child's started-by-canonical-id. The
// AgentSession reconciler re-reads that annotation every reconcile and applies
// BuildPassthroughSecretRBAC, handing the attacking session's ServiceAccount
// get+update on the NAMED user's OAuth master Secrets.
//
// So the marker must be unforgeable, not merely well-formed. Each case below is
// a way the attestation can be absent or wrong, plus the legitimate case that
// must keep working — a gate that denied everything would pass the security
// assertions and break every real continuation.
func TestReconcileRestart_TakeoverMarkerAuthorIsVerified(t *testing.T) {
	// forgedMarker is the escalation payload: a session started by the attacker
	// carrying a takeover that names the victim as the new owner.
	forgedMarker := func() *spiceboxv1alpha1.AgentSession {
		return &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p", Namespace: "ns", UID: "parent-uid",
				Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:attacker"},
			},
			Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
			Status: spiceboxv1alpha1.AgentSessionStatus{
				Phase: spiceboxv1alpha1.AgentSessionPhaseFailed,
				PendingRestart: &spiceboxv1alpha1.PendingRestart{
					Mode:              spiceboxv1alpha1.PendingRestartModeTakeover,
					NewUserText:       "continue as me",
					TriggeredBy:       "user:victim",
					TargetSessionName: "p-tk1",
					InheritHistory:    false,
					RequestedAt:       metav1.NewTime(time.Unix(1, 0)),
				},
			},
		}
	}

	// A signer the operator does not trust — a second component, or an attacker
	// who minted their own keypair and never registered it.
	untrustedSeed := make([]byte, ed25519.SeedSize)
	for i := range untrustedSeed {
		untrustedSeed[i] = 0x5c
	}
	untrusted := restartmarker.NewSigner(ed25519.NewKeyFromSeed(untrustedSeed), restartmarker.Publisher)

	cases := []struct {
		name string
		// arm prepares the marker's attestation (or leaves it absent).
		arm func(t *testing.T, sess *spiceboxv1alpha1.AgentSession)
		// wantChild is true when the fork must be allowed to materialize.
		wantChild bool
		// wantReason overrides the forgery reason for the one case that is not
		// a forgery: an absent attestation is the upgrade-window shape, refused
		// just as hard but reported as a rollout. Empty means the forgery
		// reason. See TestReconcileRestart_UnsignedMarkerIsRefusedAsARolloutNotAForgery.
		wantReason string
	}{
		{
			name:       "unsigned marker: refused, no child, RestartDenied",
			arm:        func(*testing.T, *spiceboxv1alpha1.AgentSession) {},
			wantReason: agentsession.ReasonRestartMarkerUnsigned,
		},
		{
			name: "signed by an unregistered key: refused, no child, RestartDenied",
			arm: func(t *testing.T, sess *spiceboxv1alpha1.AgentSession) {
				require.NoError(t, untrusted.Sign(sess, sess.Status.PendingRestart))
			},
		},
		{
			name: "triggeredBy rewritten after signing: refused, no child, RestartDenied",
			arm: func(t *testing.T, sess *spiceboxv1alpha1.AgentSession) {
				sess.Status.PendingRestart.TriggeredBy = "user:attacker"
				armSignedRestart(t, sess)
				// The runner keeps the connector's signature and swaps the
				// payload — the exact move a signature over the whole marker
				// has to defeat.
				sess.Status.PendingRestart.TriggeredBy = "user:victim"
			},
		},
		{
			name: "marker lifted onto a recreated session of the same name: refused, no child",
			arm: func(t *testing.T, sess *spiceboxv1alpha1.AgentSession) {
				armSignedRestart(t, sess)
				sess.UID = "a-different-uid"
			},
		},
		{
			name: "correctly signed by the connector: allowed, child created for the new owner",
			arm: func(t *testing.T, sess *spiceboxv1alpha1.AgentSession) {
				armSignedRestart(t, sess)
			},
			wantChild: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memorypkg.WithSystemApproval(context.Background(), "test")
			mem := memorypkg.NewLocal(inmem.NewBackend())
			sess := forgedMarker()
			tc.arm(t, sess)

			c := fake.NewClientBuilder().
				WithScheme(restartScheme(t)).
				WithObjects(sess, ungatedClass("ns", "demo")).
				WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
				Build()
			r := &agentsession.Reconciler{
				Client:            c,
				RestartMemory:     mem,
				AuthzGranter:      &fakeGranter{},
				Snapshotter:       &fakeSnapshotter{},
				ForkChecker:       fakeForkChecker{allow: true},
				DeniedLister:      fakeDeniedLister{},
				ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
				// Fully wired, so a refusal is about the marker's attestation
				// and never about an unwired key lookup.
				PublisherKeys: testMarkerKeys,
			}

			proceed, _, err := r.ReconcileRestart(ctx, sess)
			require.NoError(t, err, "an unverifiable marker is refused, not propagated as an error")
			assert.False(t, proceed)

			var child spiceboxv1alpha1.AgentSession
			childErr := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p-tk1"}, &child)

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))
			assert.Nil(t, got.Status.PendingRestart,
				"the marker must be cleared either way — an immortal marker wedges the thread")

			if tc.wantChild {
				require.NoError(t, childErr, "a connector-signed takeover must still materialize its child")
				assert.Equal(t, "user:victim", child.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID],
					"the legitimate takeover transfers ownership to the new user")
				assert.Equal(t, "p-tk1", got.Status.SupersededBy)
				return
			}

			assert.True(t, apierrors.IsNotFound(childErr),
				"an unverified takeover marker must create no child; got one with started-by=%q",
				child.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID])
			assert.Empty(t, got.Status.SupersededBy, "the parent must not be superseded by a forged fork")

			cond := meta.FindStatusCondition(got.Status.Conditions,
				spiceboxv1alpha1.AgentSessionConditionRestartDenied)
			require.NotNil(t, cond, "the refusal must be visible: channelsd relays this condition into the thread")
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			wantReason := tc.wantReason
			if wantReason == "" {
				wantReason = spiceboxv1alpha1.ReasonRestartMarkerUnverified
			}
			assert.Equal(t, wantReason, cond.Reason)
			assert.NotEmpty(t, cond.Message, "a denial the user cannot read is the failure mode this closes")
		})
	}
}

// TestReconcileRestart_SupersededParentStillAdjudicatesTheMarker pins the
// ORDER of ReconcileRestart's two early exits: the marker's author is
// authenticated BEFORE the already-superseded shortcut, not after.
//
// The shortcut ("this fork already happened; drop the leftover marker") is
// crash-recovery for the window between SupersedeParent and the marker clear.
// Running it first made status.supersededBy a mute button: every later marker
// on the session — including a different human's channelsd-authored TAKEOVER,
// the path that deliberately skips the SpiceDB fork gate and the recovery
// mechanism against a misbehaving runner — was nil'd by clearPendingRestart
// with no condition, no notice and no log. The webhook now refuses a runner's
// write to status.supersededBy, but the ordering is the second half: a marker
// nobody can attribute must be ADJUDICATED (visible denial) whatever else the
// status says, and only an attributable one may be quietly dropped as stale.
func TestReconcileRestart_SupersededParentStillAdjudicatesTheMarker(t *testing.T) {
	// superseded builds a session that already carries SupersededBy — the state
	// a completed fork leaves, and the state a forged patch used to fabricate —
	// plus a takeover marker naming the victim as the new owner.
	superseded := func() *spiceboxv1alpha1.AgentSession {
		return &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p", Namespace: "ns", UID: "parent-uid",
				Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:attacker"},
			},
			Status: spiceboxv1alpha1.AgentSessionStatus{
				Phase:        spiceboxv1alpha1.AgentSessionPhaseSucceeded,
				SupersededBy: "p-earlier-child",
				PendingRestart: &spiceboxv1alpha1.PendingRestart{
					Mode:              spiceboxv1alpha1.PendingRestartModeTakeover,
					NewUserText:       "continue as me",
					TriggeredBy:       "user:victim",
					TargetSessionName: "p-tk1",
					RequestedAt:       metav1.NewTime(time.Unix(1, 0)),
				},
			},
		}
	}

	cases := []struct {
		name string
		// arm prepares the marker's attestation (or leaves it absent).
		arm func(t *testing.T, sess *spiceboxv1alpha1.AgentSession)
		// wantDenied is true when the refusal must be visible on the CR.
		wantDenied bool
	}{
		{
			name: "unattributable marker on a superseded session: RestartDenied=True, not silently dropped",
			arm:  func(*testing.T, *spiceboxv1alpha1.AgentSession) {},
			// The whole point: supersededBy must not buy silence.
			wantDenied: true,
		},
		{
			name: "connector-signed marker on a superseded session: cleared quietly (crash recovery, no manufactured denial)",
			arm:  armSignedRestart,
			// A genuine leftover from an interrupted fork. Re-adjudicating it
			// WOULD only manufacture a denial notice for a completed
			// continuation, so this half of the old comment stays true.
			wantDenied: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memorypkg.WithSystemApproval(context.Background(), "test")
			sess := superseded()
			tc.arm(t, sess)

			c := fake.NewClientBuilder().
				WithScheme(restartScheme(t)).
				WithObjects(sess).
				WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
				Build()
			r := &agentsession.Reconciler{
				Client:            c,
				RestartMemory:     memorypkg.NewLocal(inmem.NewBackend()),
				AuthzGranter:      &fakeGranter{},
				Snapshotter:       &fakeSnapshotter{},
				ForkChecker:       fakeForkChecker{allow: true},
				DeniedLister:      fakeDeniedLister{},
				ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
				PublisherKeys:     testMarkerKeys,
			}

			proceed, _, err := r.ReconcileRestart(ctx, sess)
			require.NoError(t, err, "neither arm is an error path")
			assert.False(t, proceed, "a session with a marker never falls through to the normal reconcile")

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))
			assert.Nil(t, got.Status.PendingRestart,
				"the marker must be cleared either way — an immortal marker wedges the thread")
			assert.Equal(t, "p-earlier-child", got.Status.SupersededBy,
				"neither arm re-forks: the session stays superseded by its existing child")

			var child spiceboxv1alpha1.AgentSession
			assert.True(t, apierrors.IsNotFound(
				c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p-tk1"}, &child)),
				"an already-superseded session must not fork a second child")

			cond := meta.FindStatusCondition(got.Status.Conditions,
				spiceboxv1alpha1.AgentSessionConditionRestartDenied)
			if !tc.wantDenied {
				if cond != nil {
					assert.Equal(t, metav1.ConditionFalse, cond.Status,
						"a stale but attributable marker must not manufacture a denial notice")
				}
				return
			}
			require.NotNil(t, cond,
				"a marker nobody can attribute must be refused VISIBLY — channelsd relays this condition into the thread")
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			// This arm carries no attestation at all, so it is adjudicated as
			// the upgrade-window shape. Which class it lands in is incidental
			// here; that it was adjudicated by the marker gate at all is the
			// point, and the reason is what proves that.
			assert.Equal(t, agentsession.ReasonRestartMarkerUnsigned, cond.Reason)
			assert.NotEmpty(t, cond.Message, "a denial the user cannot read is the failure mode this closes")
		})
	}
}

// planGateRootFor reads the child's plan-gate records.
func planGateRootFor(t *testing.T, mem memorypkg.Memory, scope memorypkg.Scope) []plangateaudit.Content {
	t.Helper()
	recs, err := plangateaudit.List(memorypkg.WithSystemApproval(context.Background(), "test"), mem, scope)
	require.NoError(t, err)
	return recs
}

// A fork must NOT copy the parent's plan-gate chain. Those records carry a
// per-(scope, publisher) provenance sequence, so copying them gives the child a
// history it does not have — and since "a denial survives a later supersede" is
// order-dependent, a mis-ordered replay could resurrect authority a human
// revoked. The child gets ONE derived record instead.
func TestCopyMemoryPrefix_planGateRecordsAreDerivedNotCopied(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	src := memorypkg.Scope{Kind: "session", ID: "ns/parent"}
	dst := memorypkg.Scope{Kind: "session", ID: "ns/child"}

	// Seed a parent with a multi-record plan-gate history. The digest must be
	// the REAL one the reconstructed plan produces, or the fold would discard
	// every record as belonging to another plan — the failure this whole
	// derivation exists to avoid.
	idx0, idx1 := int32(0), int32(1)
	seed := []plangateaudit.Content{
		{Event: plangateaudit.EventPlanApproved, PhaseIndex: &idx0, MaxCount: 1,
			Ceiling: []string{"perm:read:tracker_issue"}, Mode: "logging"},
		{Event: plangateaudit.EventPlanApproved, PhaseIndex: &idx1, MaxCount: 1,
			Ceiling: []string{"perm:write:tracker_issue"}, Mode: "logging"},
	}
	rebuilt, ok := plangate.PlanFromRecords(withDigest(seed, "tmp"))
	require.True(t, ok)
	real := rebuilt.Digest()
	seed = withDigest(seed, real)
	seed = append(seed, plangateaudit.Content{
		Event: plangateaudit.EventPhaseSelected, PlanDigest: real, PhaseIndex: &idx1, Mode: "logging",
	})
	for _, c := range seed {
		require.NoError(t, plangateaudit.Record(ctx, mem, src, c))
	}
	require.Len(t, planGateRootFor(t, mem, src), 3)

	require.NoError(t, agentsession.CopyMemoryPrefix(ctx, mem, src, dst, 5, "next", plangate.ForkInherit))

	child := planGateRootFor(t, mem, dst)
	require.Len(t, child, 1, "the child starts from exactly one derived root, not copies")
	assert.Contains(t, child[0].Provenance, "ns/parent", "the root names where its authority came from")
	require.NotNil(t, child[0].PhaseIndex)
	assert.Equal(t, int32(1), *child[0].PhaseIndex, "the child resumes the parent's active phase")

	assert.Len(t, planGateRootFor(t, mem, src), 3, "the parent's chain stays intact and verifiable")
}

// takeover is a different owner. Inheriting would hand them the previous
// owner's human-approved ceilings on resources they never had standing on.
func TestCopyMemoryPrefix_takeoverInheritsNoPlanGateAuthority(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	src := memorypkg.Scope{Kind: "session", ID: "ns/parent"}
	dst := memorypkg.Scope{Kind: "session", ID: "ns/child"}

	idx := int32(0)
	require.NoError(t, plangateaudit.Record(ctx, mem, src, plangateaudit.Content{
		Event: plangateaudit.EventPlanApproved, PlanDigest: "d1", PhaseIndex: &idx,
		Ceiling: []string{"perm:write:tracker_issue"}, Mode: "logging",
	}))

	require.NoError(t, agentsession.CopyMemoryPrefix(ctx, mem, src, dst, 5, "mine now", plangate.ForkTakeover))

	child := planGateRootFor(t, mem, dst)
	require.Len(t, child, 1)
	assert.Empty(t, child[0].Ceiling, "a taken-over session inherits no reach")
	assert.Empty(t, child[0].PlanDigest, "and no plan")
}

// A session that never used the plan gate must fork exactly as before.
func TestCopyMemoryPrefix_noPlanGateHistoryWritesNoRoot(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	mem := memorypkg.NewLocal(inmem.NewBackend())
	src := memorypkg.Scope{Kind: "session", ID: "ns/parent"}
	dst := memorypkg.Scope{Kind: "session", ID: "ns/child"}

	require.NoError(t, agentsession.CopyMemoryPrefix(ctx, mem, src, dst, -1, "hello", plangate.ForkInherit))

	assert.Empty(t, planGateRootFor(t, mem, dst),
		"a session that never used the gate must fork exactly as it did before")
}

// withDigest stamps a plan digest onto every record, so a fixture can be built
// then re-pointed at the digest its own reconstruction produces.
func withDigest(in []plangateaudit.Content, digest string) []plangateaudit.Content {
	out := make([]plangateaudit.Content, len(in))
	for i, c := range in {
		c.PlanDigest = digest
		out[i] = c
	}
	return out
}
