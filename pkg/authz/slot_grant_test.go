package authz_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

type recordingRelWriter struct {
	wrote   []authz.Relation
	deleted []authz.Relation
	err     error
}

func (r *recordingRelWriter) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	if r.err != nil {
		return r.err
	}
	r.wrote = append(r.wrote, rels...)
	return nil
}

func (r *recordingRelWriter) DeleteRelationships(_ context.Context, rels []authz.Relation) error {
	if r.err != nil {
		return r.err
	}
	r.deleted = append(r.deleted, rels...)
	return nil
}

// pinningFake implements BOTH authz.RelWriter and authz.SlotPinner — the pair a
// single-occupancy grant needs, and what recordingRelWriter deliberately is not
// (recordingRelWriter is kept for the fail-closed "writer is not a pinner"
// case). Every grant it records lands in `wrote`, whether it arrived through the
// pinned write (single types) or the plain batched write (multi types), so a
// test that only inspects `wrote` reads the same whichever path a binding took.
// `pins` holds one pinned instance per (scope,type), surviving across calls so
// idempotent re-grants of the same instance behave as production does.
type pinningFake struct {
	wrote     []authz.Relation
	deleted   []authz.Relation
	plain     []authz.Relation // every WriteRelationships (multi) rel, in order
	pinned    []pinnedWrite    // every WriteGrantsPinned call, in order
	pins      map[string]string
	grantsFor map[string][]authz.Relation // seeded per type+"\x00"+id, served by ListGrantsFor
	ensureErr error                       // injected into EnsurePin
	writeErr  error                       // injected into every write method
}

type pinnedWrite struct {
	resourceType string
	pinnedID     string
	rels         []authz.Relation
}

func pinKey(scope authz.SessionRef, resourceType string) string {
	return scope.String() + "\x00" + resourceType
}

func (f *pinningFake) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.wrote = append(f.wrote, rels...)
	f.plain = append(f.plain, rels...)
	return nil
}

func (f *pinningFake) DeleteRelationships(_ context.Context, rels []authz.Relation) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.deleted = append(f.deleted, rels...)
	return nil
}

func (f *pinningFake) EnsurePin(_ context.Context, resourceType, resourceID string, scope authz.SessionRef) (bool, string, error) {
	if f.ensureErr != nil {
		return false, "", f.ensureErr
	}
	if f.pins == nil {
		f.pins = map[string]string{}
	}
	key := pinKey(scope, resourceType)
	if cur, ok := f.pins[key]; ok {
		return true, cur, nil
	}
	f.pins[key] = resourceID
	return false, resourceID, nil
}

func (f *pinningFake) WriteGrantsPinned(_ context.Context, rels []authz.Relation, resourceType, pinnedID string, _ authz.SessionRef) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.wrote = append(f.wrote, rels...)
	f.pinned = append(f.pinned, pinnedWrite{resourceType: resourceType, pinnedID: pinnedID, rels: rels})
	return nil
}

func (f *pinningFake) MovePin(_ context.Context, resourceType, _, toID string, revoke []authz.Relation, scope authz.SessionRef) error {
	if f.pins == nil {
		f.pins = map[string]string{}
	}
	f.pins[pinKey(scope, resourceType)] = toID
	f.deleted = append(f.deleted, revoke...)
	return nil
}

func (f *pinningFake) ReadPin(_ context.Context, resourceType string, scope authz.SessionRef) (string, error) {
	return f.pins[pinKey(scope, resourceType)], nil
}

func (f *pinningFake) ListGrantsFor(_ context.Context, resourceType, resourceID string, _ authz.SessionRef) ([]authz.Relation, error) {
	return f.grantsFor[resourceType+"\x00"+resourceID], nil
}

// seedPin pre-fills a type's pin, for the tests that drive a second arrival
// against an already-occupied slot.
func (f *pinningFake) seedPin(scope authz.SessionRef, resourceType, resourceID string) {
	if f.pins == nil {
		f.pins = map[string]string{}
	}
	f.pins[pinKey(scope, resourceType)] = resourceID
}

var (
	_ authz.RelWriter  = (*pinningFake)(nil)
	_ authz.SlotPinner = (*pinningFake)(nil)
)

func testScope() authz.SessionRef { return authz.SessionRef{Namespace: "default", Name: "sess-1"} }

// testExpiry is any non-zero expiry; GrantSlots refuses a zero one because the
// schema declares slot_grant `with expiration`.
func testExpiry() time.Time { return time.Unix(1700000000, 0).UTC() }

// The tuple points RESOURCE → SESSION. Asserted explicitly because the mirror
// image is a real shape in this codebase (the session-grant mechanism slots
// replace), and getting the direction backwards would still compile, still
// write a tuple, and silently reintroduce the need for a wildcard leaf to make
// any Check pass.
func TestSlotGrantRelation_pointsFromResourceToSession(t *testing.T) {
	rel := authz.SlotGrantRelation("crm_company", "acme", "contact_access", testScope())

	assert.Equal(t, "crm_company", rel.ResourceType, "the RESOURCE holds the tuple")
	assert.Equal(t, "acme", rel.ResourceID)
	assert.Equal(t, authz.SlotGrantRelationName("contact_access"), rel.Relation,
		"per-permission: a read grant must not be the same tuple as a write grant")
	assert.Equal(t, "agentsession", rel.SubjectType, "and the SESSION is the subject")
	assert.Equal(t, "default/sess-1", rel.SubjectID)
	assert.Empty(t, rel.SubjectRel,
		"the subject is the session object itself; the schema arrow (->interact) resolves its members")
}

func TestGrantSlots_writesOnePerBinding(t *testing.T) {
	// Two DISTINCT single-occupancy types, so each pins its own instance and
	// both grants land — the per-type path, not a multi-arrival.
	w := &pinningFake{}
	require.NoError(t, authz.GrantSlots(context.Background(), w, testScope(), []authz.SlotBinding{
		{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"},
		{ResourceType: "tracker_issue", ResourceID: authz.TrustedObjectID("L-140"), Permission: "write"},
	}, testExpiry()))

	require.Len(t, w.wrote, 2)
	assert.Equal(t, "acme", w.wrote[0].ResourceID)
	assert.Equal(t, "L-140", w.wrote[1].ResourceID)
	for _, r := range w.wrote {
		assert.Equal(t, "default/sess-1", r.SubjectID, "every grant binds to the same session")
	}
}

// An empty id would write a tuple naming no instance: it grants nothing and
// cannot be revoked by id afterwards. Refused before any write, so a bad entry
// cannot leave a partial set behind.
func TestGrantSlots_refusesAnEmptyIdentifierWithoutWritingAnything(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding authz.SlotBinding
	}{
		{"empty resourceID", authz.SlotBinding{ResourceType: "crm_company", Permission: "contact_access"}},
		{"empty resourceType", authz.SlotBinding{ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"}},
	} {
		t.Run(tc.name+": refused", func(t *testing.T) {
			w := &recordingRelWriter{}
			err := authz.GrantSlots(context.Background(), w, testScope(), []authz.SlotBinding{
				{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("good"), Permission: "contact_access"},
				tc.binding,
			}, testExpiry())
			require.Error(t, err)
			assert.Empty(t, w.wrote,
				"nothing may be written when any binding in the set is malformed")
		})
	}
}

// A write failure must surface. A silently-dropped grant leaves the agent
// believing it holds reach it does not, which shows up later as an inexplicable
// mid-task denial rather than a failure at bind time.
func TestGrantSlots_propagatesTheWriteError(t *testing.T) {
	// A single-occupancy grant routes through the pinner, so the store failure is
	// injected there; the point is unchanged — a dropped grant must not look like
	// a success.
	w := &pinningFake{writeErr: errors.New("spicedb unavailable")}
	err := authz.GrantSlots(context.Background(), w, testScope(),
		[]authz.SlotBinding{{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"}}, testExpiry())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spicedb unavailable")
}

// Every grant that can be written must be revocable, or a binding is a one-way
// door for the life of the session.
func TestRevokeSlots_deletesTheSameTupleItWould(t *testing.T) {
	w := &pinningFake{}
	binding := []authz.SlotBinding{{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"}}

	require.NoError(t, authz.GrantSlots(context.Background(), w, testScope(), binding, testExpiry()))
	require.NoError(t, authz.RevokeSlots(context.Background(), w, testScope(), binding))

	require.Len(t, w.wrote, 1)
	require.Len(t, w.deleted, 1)

	// Compared on IDENTITY, not the whole struct: a tuple is identified by
	// (resource, relation, subject), and the expiry is not part of that. A
	// revoker must be able to remove a grant without knowing when it was due
	// to lapse — requiring the expiry to match would make a grant unrevocable
	// by anyone who did not write it.
	got, want := w.deleted[0], w.wrote[0]
	want.ExpiresAt = got.ExpiresAt
	assert.Equal(t, want, got,
		"revoke must target exactly the tuple grant wrote, or it silently removes nothing")
	assert.True(t, got.ExpiresAt.IsZero(), "revoke names no expiry; it is not part of tuple identity")
}

// Nil writer / empty set are no-ops rather than errors: a class declaring no
// slots is the common case and must not need a guard at every call site.
func TestGrantSlots_nilWriterAndEmptySetAreNoOps(t *testing.T) {
	require.NoError(t, authz.GrantSlots(context.Background(), nil, testScope(),
		[]authz.SlotBinding{{ResourceType: "x", ResourceID: authz.TrustedObjectID("y"), Permission: "p"}}, testExpiry()))

	w := &recordingRelWriter{}
	require.NoError(t, authz.GrantSlots(context.Background(), w, testScope(), nil, testExpiry()))
	assert.Empty(t, w.wrote)
}

// TestGrantSlots_RefusesAZeroExpiry: the composed schema declares slot_grant
// `with expiration`, so SpiceDB would reject the write anyway — the point of
// catching it here is that the caller learns WHY instead of getting a caveat
// error from three layers down. An indefinite slot grant is the leak the
// expiry exists to backstop, so it must not be expressible.
func TestGrantSlots_RefusesAZeroExpiry(t *testing.T) {
	w := &recordingRelWriter{}
	err := authz.GrantSlots(context.Background(), w, testScope(),
		[]authz.SlotBinding{{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"}}, time.Time{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expiry")
	assert.Empty(t, w.wrote, "nothing may be written without an expiry")
}

func TestGrantSlots_StampsTheExpiryOnEveryTuple(t *testing.T) {
	w := &pinningFake{}
	exp := testExpiry()
	require.NoError(t, authz.GrantSlots(context.Background(), w, testScope(), []authz.SlotBinding{
		{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"},
		{ResourceType: "tracker_issue", ResourceID: authz.TrustedObjectID("L-140"), Permission: "write"},
	}, exp))

	require.Len(t, w.wrote, 2)
	for _, r := range w.wrote {
		assert.Equal(t, exp, r.ExpiresAt, "every grant carries the expiry, not just the first")
	}
}

// GrantSlots must refuse a binding whose id was never derived, because a zero
// ObjectID names no instance and the resulting tuple could not be revoked.
func TestGrantSlots_RefusesAZeroObjectID(t *testing.T) {
	w := &recordingRelWriter{}
	err := authz.GrantSlots(context.Background(), w,
		authz.SessionRef{Namespace: "default", Name: "demo-session"},
		[]authz.SlotBinding{{ResourceType: "git_repo", Permission: "push"}}, // ResourceID left zero
		time.Now().Add(time.Hour))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resourceID")
	assert.Empty(t, w.wrote, "nothing may be written when any binding is invalid")
}

func TestGrantSlots_WritesTheDerivedID(t *testing.T) {
	id, err := authz.NewObjectID("https://github.com/acme/app", []string{"normalize_url", "spicedb_escape"})
	require.NoError(t, err)

	w := &pinningFake{}
	require.NoError(t, authz.GrantSlots(context.Background(), w,
		authz.SessionRef{Namespace: "default", Name: "demo-session"},
		[]authz.SlotBinding{{ResourceType: "git_repo", ResourceID: id, Permission: "push"}},
		time.Now().Add(time.Hour)))

	require.Len(t, w.wrote, 1)
	assert.Equal(t, "https=3A//github=2Ecom/acme/app", w.wrote[0].ResourceID,
		"the escaped form reaches SpiceDB, never the raw URL")
	assert.Equal(t, "slot_grant_push", w.wrote[0].Relation)
}

// --- single-occupancy pinning gate ---

// The first instance to arrive for a single-occupancy type pins the slot and
// grants THROUGH the pin, not through the plain write — the pinned write is
// what guards the grant against a concurrent move.
func TestGrantSlots_SingleOccupancy_FirstInstancePinsThenGrants(t *testing.T) {
	w := &pinningFake{}
	scope := testScope()
	require.NoError(t, authz.GrantSlots(context.Background(), w, scope,
		[]authz.SlotBinding{{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("acme/app"), Permission: "push"}},
		testExpiry()))

	assert.Equal(t, "acme/app", w.pins[pinKey(scope, "git_repo")], "the first instance must pin the slot")

	require.Len(t, w.pinned, 1, "a single-occupancy grant must route through WriteGrantsPinned")
	assert.Equal(t, "acme/app", w.pinned[0].pinnedID, "the grant is written against the pinned id")
	assert.Empty(t, w.plain, "nothing may take the unpinned plain path for a single-occupancy type")
	require.Len(t, w.wrote, 1)
	assert.Equal(t, "acme/app", w.wrote[0].ResourceID)
	assert.Equal(t, authz.SlotGrantRelationName("push"), w.wrote[0].Relation)
}

// A second permission on the SAME pinned instance is not drift — it binds, and
// the pin is untouched. (Seeded pin == the slot already filled by this instance.)
func TestGrantSlots_SingleOccupancy_SameInstanceSecondPermissionBinds(t *testing.T) {
	w := &pinningFake{}
	scope := testScope()
	w.seedPin(scope, "git_repo", "acme/app")

	require.NoError(t, authz.GrantSlots(context.Background(), w, scope,
		[]authz.SlotBinding{{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("acme/app"), Permission: "read"}},
		testExpiry()))

	require.Len(t, w.wrote, 1, "a second permission on the same pinned instance still binds")
	assert.Equal(t, authz.SlotGrantRelationName("read"), w.wrote[0].Relation)
	assert.Equal(t, "acme/app", w.pins[pinKey(scope, "git_repo")], "the pin is unchanged")
}

// A bind to a DIFFERENT instance of a filled single-occupancy slot is refused,
// errors.Is-ably, naming the pinned instance — and the route out depends on the
// slot's rebind mode. These two message texts are the spec's, implemented and
// asserted here.
func TestGrantSlots_SingleOccupancy_DifferentInstanceRefused(t *testing.T) {
	scope := testScope()
	cases := []struct {
		name    string
		rebind  string
		wantMsg string
	}{
		{name: "rebind=approval: refusal routes to a plan amendment", rebind: "approval", wantMsg: "plan"},
		{name: "rebind unset: defaults to the approval routing", rebind: "", wantMsg: "plan"},
		{name: "rebind=never: refusal routes to a new session", rebind: "never", wantMsg: "new session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &pinningFake{}
			w.seedPin(scope, "git_repo", "acme/app") // pinned to a DIFFERENT instance

			err := authz.GrantSlots(context.Background(), w, scope,
				[]authz.SlotBinding{{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("other/repo"), Permission: "push", Rebind: tc.rebind}},
				testExpiry())

			require.Error(t, err)
			assert.ErrorIs(t, err, authz.ErrSlotPinned)
			assert.Contains(t, err.Error(), "acme/app", "the refusal must name the pinned instance")
			assert.Contains(t, err.Error(), tc.wantMsg)
			assert.Empty(t, w.wrote, "a refused rebind must grant nothing")
			assert.Equal(t, "acme/app", w.pins[pinKey(scope, "git_repo")], "the pin must not move")
		})
	}
}

// Two distinct instances of ONE single-occupancy type in one call cannot be
// reconciled to a single occupant: that type is refused, pinning and granting
// nothing — but a DIFFERENT type's binding in the same call still lands. This is
// the per-type partition: one refused type must not strand another's grant.
func TestGrantSlots_SingleOccupancy_MultiArrivalRefusedButOtherTypesLand(t *testing.T) {
	w := &pinningFake{}
	scope := testScope()
	err := authz.GrantSlots(context.Background(), w, scope, []authz.SlotBinding{
		{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("acme/app"), Permission: "push"},
		{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("acme/other"), Permission: "push"},
		{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"},
	}, testExpiry())

	require.Error(t, err)
	assert.ErrorIs(t, err, authz.ErrSlotPinned)
	assert.Contains(t, err.Error(), "git_repo", "the refusal must name the over-subscribed type")

	_, pinned := w.pins[pinKey(scope, "git_repo")]
	assert.False(t, pinned, "a multi-arrival must pin nothing for that type")
	for _, r := range w.wrote {
		assert.NotEqual(t, "git_repo", r.ResourceType, "nothing may be granted for the refused type")
	}

	require.Len(t, w.wrote, 1, "the independent type's grant must survive the refusal")
	assert.Equal(t, "crm_company", w.wrote[0].ResourceType)
	assert.Equal(t, "acme", w.pins[pinKey(scope, "crm_company")], "and it pins on its own")
}

// Two DISTINCT single-occupancy types, each already pinned to a different
// instance, are BOTH refused in one call — and both refusals must surface.
// Keeping only the first (the pre-errors.Join behaviour) would tell the caller
// about one blocked type and leave the second to be rediscovered by trying it.
// errors.Join keeps errors.Is(ErrSlotPinned) reachable and both sentences
// present.
func TestGrantSlots_SingleOccupancy_EveryRefusalSurfaces(t *testing.T) {
	w := &pinningFake{}
	scope := testScope()
	w.seedPin(scope, "git_repo", "acme/app")
	w.seedPin(scope, "crm_company", "acme")

	err := authz.GrantSlots(context.Background(), w, scope, []authz.SlotBinding{
		{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("other/repo"), Permission: "push"},
		{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("globex"), Permission: "contact_access"},
	}, testExpiry())

	require.Error(t, err)
	assert.ErrorIs(t, err, authz.ErrSlotPinned, "errors.Join must keep the sentinel reachable across both refusals")
	assert.Contains(t, err.Error(), "git_repo:acme/app", "the FIRST type's refusal must be present")
	assert.Contains(t, err.Error(), "crm_company:acme", "the SECOND type's refusal must not be dropped")
	assert.Empty(t, w.wrote, "both types refused; nothing is granted")
}

// A pin refusal AND a failed plain (multi) write in the SAME call must BOTH
// surface: the write error must join the pending refusal, not replace it. Before
// errors.Join the plain-write error path returned immediately and discarded a
// refusal already accumulated for another type.
func TestGrantSlots_RefusalAndPlainWriteErrorBothSurface(t *testing.T) {
	w := &pinningFake{writeErr: errors.New("spicedb unavailable")}
	scope := testScope()
	w.seedPin(scope, "git_repo", "acme/app") // single type pinned elsewhere → refused, no write attempted

	err := authz.GrantSlots(context.Background(), w, scope, []authz.SlotBinding{
		{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("other/repo"), Permission: "push"},
		{ResourceType: "label", ResourceID: authz.TrustedObjectID("bug"), Permission: "apply", Occupancy: "multi"},
	}, testExpiry())

	require.Error(t, err)
	assert.ErrorIs(t, err, authz.ErrSlotPinned, "the pin refusal must not be swallowed by the plain-write error")
	assert.Contains(t, err.Error(), "spicedb unavailable", "the plain-write error must also surface")
	assert.Contains(t, err.Error(), "git_repo:acme/app", "the refusal text must still be present alongside the write error")
}

// A STORE failure on one single-occupancy type must not abort the call: the
// multi-occupancy write still lands and the failure still surfaces. Returning
// on the first EnsurePin error skipped every type after it — grants a human may
// already have approved.
func TestGrantSlots_PinStoreErrorDoesNotStrandOtherTypes(t *testing.T) {
	w := &pinningFake{ensureErr: errors.New("spicedb unavailable")}
	scope := testScope()

	err := authz.GrantSlots(context.Background(), w, scope, []authz.SlotBinding{
		{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("acme/app"), Permission: "push"},
		{ResourceType: "label", ResourceID: authz.TrustedObjectID("bug"), Permission: "apply", Occupancy: "multi"},
	}, testExpiry())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ensure pin for git_repo:acme/app", "the store failure must surface")
	assert.NotErrorIs(t, err, authz.ErrSlotPinned, "a store failure is not a pin refusal")
	require.Len(t, w.plain, 1, "the multi-occupancy type after it must still be written")
	assert.Equal(t, "label", w.plain[0].ResourceType)
}

// A plain RelWriter that is not a SlotPinner cannot express the pin's
// precondition. The gate refuses the single-occupancy type rather than writing
// the grant through unpinned as if it were unmarked. recordingRelWriter is the
// deliberately-non-pinner writer this case needs.
func TestGrantSlots_SingleOccupancy_NonPinnerWriterRefusesWithoutWriting(t *testing.T) {
	w := &recordingRelWriter{}
	err := authz.GrantSlots(context.Background(), w, testScope(),
		[]authz.SlotBinding{{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("acme/app"), Permission: "push"}},
		testExpiry())
	require.Error(t, err, "a single-occupancy type cannot bind through a writer that is not a SlotPinner")
	assert.Empty(t, w.wrote, "nothing may be written when the pin cannot be expressed")
}

// A multi-occupancy slot binds a SET: every addition lands through the plain
// batched write, and NO pin is written. This is today's behavior, unchanged.
func TestGrantSlots_MultiOccupancy_BindsAsetWithoutPinning(t *testing.T) {
	w := &pinningFake{}
	require.NoError(t, authz.GrantSlots(context.Background(), w, testScope(), []authz.SlotBinding{
		{ResourceType: "label", ResourceID: authz.TrustedObjectID("bug"), Permission: "apply", Occupancy: "multi"},
		{ResourceType: "label", ResourceID: authz.TrustedObjectID("urgent"), Permission: "apply", Occupancy: "multi"},
	}, testExpiry()))

	require.Len(t, w.wrote, 2, "a multi-occupancy slot binds a set; each addition lands")
	assert.Empty(t, w.pinned, "multi occupancy must NOT route through the pinned write")
	assert.Empty(t, w.pins, "multi occupancy writes no pin")
	assert.Len(t, w.plain, 2, "multi grants take the plain batched write")
}

// SlotGrantExpiry anchors to the session's WALL-CLOCK cap, and a session that
// declares none must still get a bounded grant — zero is not "forever".
func TestSlotGrantExpiry(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	assert.Equal(t, now.Add(2*time.Hour), authz.SlotGrantExpiry(now, 2*time.Hour),
		"a declared cap is used as-is")
	assert.Equal(t, now.Add(authz.DefaultSlotGrantTTL), authz.SlotGrantExpiry(now, 0),
		"no declared cap must NOT mean no expiry")
	assert.Equal(t, now.Add(authz.DefaultSlotGrantTTL), authz.SlotGrantExpiry(now, -time.Hour),
		"a negative cap is nonsense and must not produce a past expiry")
}

// fakeSlotCopier records what a fork carried. held/pins are the parent's
// state the fake hands back; copied is every relation CopySlotGrants asked it
// to write, verbatim — the single seam the gate-bypassing copy writes through.
type fakeSlotCopier struct {
	held    []authz.SlotBinding
	pins    []authz.Relation
	listErr error
	pinsErr error
	copyErr error
	copied  []authz.Relation
}

func (f *fakeSlotCopier) ListSlotGrants(_ context.Context, ns, name string) ([]authz.SlotBinding, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.held, nil
}

func (f *fakeSlotCopier) ListSlotPins(_ context.Context, ns, name string) ([]authz.Relation, error) {
	if f.pinsErr != nil {
		return nil, f.pinsErr
	}
	return f.pins, nil
}

func (f *fakeSlotCopier) CopySlotTuples(_ context.Context, rels []authz.Relation) error {
	if f.copyErr != nil {
		return f.copyErr
	}
	f.copied = append(f.copied, rels...)
	return nil
}

func TestCopySlotGrants_RePointsTheParentsGrantsAtTheChild(t *testing.T) {
	c := &fakeSlotCopier{held: []authz.SlotBinding{
		{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"},
		{ResourceType: "http_target", ResourceID: authz.TrustedObjectID("hash-1"), Permission: "reachable"},
	}}
	parent := authz.SessionRef{Namespace: "ns", Name: "parent"}
	childRef := authz.SessionRef{Namespace: "ns", Name: "child"}
	exp := testExpiry()

	n, err := authz.CopySlotGrants(context.Background(), c, parent, childRef, exp)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	require.Len(t, c.copied, 2)
	for _, rel := range c.copied {
		assert.Equal(t, "ns/child", rel.SubjectID,
			"a grant names the SESSION as subject, so a continuation inherits nothing without re-pointing")
		assert.Equal(t, exp, rel.ExpiresAt, "the child's grants get the child's expiry, not the parent's remaining time")
	}
	assert.ElementsMatch(t, []string{"acme", "hash-1"}, []string{c.copied[0].ResourceID, c.copied[1].ResourceID})
}

func TestCopySlotGrants_NoGrantsAndNilCopierAreNoOps(t *testing.T) {
	parent := authz.SessionRef{Namespace: "ns", Name: "parent"}
	childRef := authz.SessionRef{Namespace: "ns", Name: "child"}

	n, err := authz.CopySlotGrants(context.Background(), nil, parent, childRef, testExpiry())
	require.NoError(t, err)
	assert.Zero(t, n)

	empty := &fakeSlotCopier{}
	n, err = authz.CopySlotGrants(context.Background(), empty, parent, childRef, testExpiry())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, empty.copied, "nothing held and no pin means nothing written")
}

// TestCopySlotGrants_CopiesThePinVerbatim is brief step 1(a): a parent holding
// a pin must leave the child pinned to the SAME instance — copied byte for
// byte, re-targeted only at the subject, never re-derived from GrantSlots.
func TestCopySlotGrants_CopiesThePinVerbatim(t *testing.T) {
	parent := authz.SessionRef{Namespace: "ns", Name: "parent"}
	childRef := authz.SessionRef{Namespace: "ns", Name: "child"}
	exp := testExpiry()

	c := &fakeSlotCopier{
		held: []authz.SlotBinding{
			{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("acme/app"), Permission: "push"},
		},
		pins: []authz.Relation{authz.SlotPinRelation("git_repo", "acme/app", parent)},
	}

	n, err := authz.CopySlotGrants(context.Background(), c, parent, childRef, exp)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.Len(t, c.copied, 2, "the grant and the pin both copy in the same call")

	var gotPin, gotGrant *authz.Relation
	for i := range c.copied {
		rel := &c.copied[i]
		if rel.Relation == authz.SlotPinRelationName {
			gotPin = rel
		} else {
			gotGrant = rel
		}
	}
	require.NotNil(t, gotPin, "the pin must be among the copied tuples")
	require.NotNil(t, gotGrant, "the grant must be among the copied tuples")

	assert.Equal(t, "acme/app", gotPin.ResourceID)
	assert.Equal(t, childRef.String(), gotPin.SubjectID, "the pin moves to the CHILD, verbatim, bypassing the gate")
	assert.True(t, gotPin.ExpiresAt.IsZero(), "a pin never expires")

	assert.Equal(t, "acme/app", gotGrant.ResourceID)
	assert.Equal(t, childRef.String(), gotGrant.SubjectID)
	assert.Equal(t, exp, gotGrant.ExpiresAt, "the grant keeps the caller-stamped child expiry")
}

// TestCopySlotGrants_TwoInstancesOfOneTypeCopyVerbatimWithoutGating is brief
// step 1(b): a parent holding TWO instances of one type — a multi slot's
// restart, or a legacy session recorded before single-occupancy pinning
// existed — must copy both grants and pin nothing. Routing this through
// GrantSlots instead would refuse the whole type (ErrSlotPinned: a
// single-occupancy type cannot hold 2 distinct instances), because a tuple
// read back from SpiceDB carries no Occupancy for the gate to read as "multi".
// That refusal, on a lifecycle clone rather than a new arrival, is the bug
// this verbatim copy fixes.
func TestCopySlotGrants_TwoInstancesOfOneTypeCopyVerbatimWithoutGating(t *testing.T) {
	parent := authz.SessionRef{Namespace: "ns", Name: "parent"}
	childRef := authz.SessionRef{Namespace: "ns", Name: "child"}
	exp := testExpiry()

	c := &fakeSlotCopier{held: []authz.SlotBinding{
		{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("acme/app"), Permission: "push"},
		{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("acme/other"), Permission: "push"},
	}}

	n, err := authz.CopySlotGrants(context.Background(), c, parent, childRef, exp)
	require.NoError(t, err, "a verbatim copy must not re-gate occupancy")
	assert.Equal(t, 2, n)
	require.Len(t, c.copied, 2)
	for _, rel := range c.copied {
		assert.NotEqual(t, authz.SlotPinRelationName, rel.Relation, "two legacy/multi instances must pin nothing")
		assert.Equal(t, childRef.String(), rel.SubjectID)
	}
	assert.ElementsMatch(t, []string{"acme/app", "acme/other"}, []string{c.copied[0].ResourceID, c.copied[1].ResourceID})
}

func TestCopySlotGrants_SurfacesEveryFailure(t *testing.T) {
	parent := authz.SessionRef{Namespace: "ns", Name: "parent"}
	childRef := authz.SessionRef{Namespace: "ns", Name: "child"}
	binding := []authz.SlotBinding{{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"}}

	cases := []struct {
		name    string
		copier  *fakeSlotCopier
		wantMsg string
	}{
		{name: "listing the parent's grants fails", copier: &fakeSlotCopier{listErr: errors.New("list boom")}, wantMsg: "list boom"},
		{name: "listing the parent's pins fails", copier: &fakeSlotCopier{held: binding, pinsErr: errors.New("pins boom")}, wantMsg: "pins boom"},
		{name: "the copy write fails", copier: &fakeSlotCopier{held: binding, copyErr: errors.New("copy boom")}, wantMsg: "copy boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := authz.CopySlotGrants(context.Background(), tc.copier, parent, childRef, testExpiry())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// fakeRevokeChecker drives the two standing legs independently.
type fakeRevokeChecker struct {
	approve, admin       bool
	approveErr, adminErr error
	approveFC, adminFC   bool
	adminPerm            string
}

func (f *fakeRevokeChecker) CheckApprove(_ context.Context, _, _ string, _ identity.CanonicalUserID, fc bool) (bool, error) {
	f.approveFC = fc
	return f.approve, f.approveErr
}

func (f *fakeRevokeChecker) CheckPlatformPermission(_ context.Context, perm string, _ identity.CanonicalUserID, fc bool) (bool, error) {
	f.adminPerm, f.adminFC = perm, fc
	return f.admin, f.adminErr
}

// Standing to revoke is symmetric with standing to approve, plus platform
// admin. A rule where approving is easier than un-approving leaves people
// unable to undo their own decisions — and then they stop approving carefully.
func TestCheckMayRevokeSlot(t *testing.T) {
	who := identity.CanonicalFromTrusted("alice", "test fixture")
	cases := []struct {
		name string
		c    *fakeRevokeChecker
		want bool
		err  bool
	}{
		{name: "approver may revoke", c: &fakeRevokeChecker{approve: true}, want: true},
		{name: "platform admin may revoke without approve standing", c: &fakeRevokeChecker{admin: true}, want: true},
		{name: "neither: refused", c: &fakeRevokeChecker{}, want: false},
		{
			name: "approve check errors: fail closed, and say why",
			c:    &fakeRevokeChecker{approveErr: errors.New("spicedb down")},
			want: false, err: true,
		},
		{
			// Reached only when approve already said no, so a broken admin
			// check must not quietly grant.
			name: "platform check errors: fail closed",
			c:    &fakeRevokeChecker{adminErr: errors.New("spicedb down")},
			want: false, err: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := authz.CheckMayRevokeSlot(context.Background(), tc.c, testScope(), who)
			if tc.err {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCheckMayRevokeSlot_NoCheckerRefuses(t *testing.T) {
	got, err := authz.CheckMayRevokeSlot(context.Background(), nil, testScope(), identity.CanonicalFromTrusted("alice", "test fixture"))
	require.Error(t, err, "an unwired gate must refuse loudly, not silently allow")
	assert.False(t, got)
}

// Both legs read FULLY CONSISTENT: a caller who was just granted approve
// standing must not be told they lack it, and standing withdrawn a moment ago
// must not still authorize.
func TestCheckMayRevokeSlot_ReadsFullyConsistent(t *testing.T) {
	c := &fakeRevokeChecker{}
	_, err := authz.CheckMayRevokeSlot(context.Background(), c, testScope(), identity.CanonicalFromTrusted("alice", "test fixture"))
	require.NoError(t, err)
	assert.True(t, c.approveFC, "approve leg must be fully consistent")
	assert.True(t, c.adminFC, "platform leg must be fully consistent")
	assert.Equal(t, authz.PlatformKillSession, c.adminPerm,
		"must go through the per-area permission, never can_admin directly")
}
