package relsync_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// fakePassKind is a Pass-oriented relsync.Kind fixture: pages served off a
// slice, addressed by CURSOR TOKEN rather than an ever-incrementing index —
// the zero Cursor always serves pages[0], and a non-zero cursor's Token is
// the target page index (a plain decimal string, set via ScopePage.Next in
// each test fixture). This makes ListScopes stateless with respect to
// previous calls, matching real pagination: a fresh Pass() call restarts at
// Cursor{} and gets pages[0] again, which TestPass_ConvergesAcrossRepeatedPassesWithASmallBudget
// depends on (it calls Pass repeatedly against the same k, exactly as the
// controller would across reconciles).
//
// Per-scope fetch results/errors/join-miss counts are keyed by ScopeID.
//
// Named distinctly from kind_test.go's own `fakeKind` (a minimal
// value-typed registry fixture) — both live in this same
// package relsync_test, so the two names can't collide.
type fakePassKind struct {
	mu sync.Mutex

	pages []relsync.ScopePage

	members    map[relsync.ScopeID][]spicedb.Tuple
	joinMisses map[relsync.ScopeID]int
	fetchErrs  map[relsync.ScopeID]error

	// claims overrides the relsource claims this fixture's Source
	// declares. Nil means defaultFakeClaims.
	claims []string

	listCalls  int
	fetchCalls []relsync.ScopeID
}

func (k *fakePassKind) Name() string { return "fakepass" }

// defaultFakeClaims is what a fixture claims when it does not say: every
// relation the slack-shaped fixtures in this file write. Claims are load
// bearing here, not decoration — the cross-resource reap deletes only
// relations the source itself claims (see reapAbsentCrossResourceTuples),
// so a fixture declaring none would model a source permitted to write
// nothing, and would reap nothing.
var defaultFakeClaims = []string{
	"slack_channel#member",
	"slack_channel#relhash",
	"slack_user#user",
}

func (k *fakePassKind) Source() relsource.Source {
	claims := k.claims
	if claims == nil {
		claims = defaultFakeClaims
	}
	return relsource.Source{Name: "fakepasssync", Claims: claims}
}

func (k *fakePassKind) ListScopes(_ context.Context, _ relsync.SourceParams, after relsync.Cursor) (relsync.ScopePage, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.listCalls++

	idx := 0
	if after != (relsync.Cursor{}) {
		n, err := strconv.Atoi(after.Token)
		if err != nil {
			return relsync.ScopePage{}, fmt.Errorf("fakePassKind: bad cursor token %q: %w", after.Token, err)
		}
		idx = n
	}
	if idx >= len(k.pages) {
		// No more pages configured: a clean, complete, empty end. Every test
		// below configures exactly as many pages as it expects Pass to
		// request.
		return relsync.ScopePage{Complete: true}, nil
	}
	return k.pages[idx], nil
}

func (k *fakePassKind) FetchScope(_ context.Context, _ relsync.SourceParams, s relsync.Scope) (relsync.ScopeContent, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.fetchCalls = append(k.fetchCalls, s.ID)
	if err, ok := k.fetchErrs[s.ID]; ok {
		return relsync.ScopeContent{}, err
	}
	return relsync.ScopeContent{Tuples: k.members[s.ID], JoinMisses: k.joinMisses[s.ID]}, nil
}

// fakeWriter records every WriteRelationships/DeleteRelationships call
// verbatim — never a hand-summarized count — so a test can assert on the
// exact calls made. This is the whole reason for existing: a test that
// asserted only on PassResult would still pass if a delete were added
// later; asserting on w.deletes/w.writes catches that a delete happened
// (or didn't) regardless of what the result summary claims.
type fakeWriter struct {
	mu sync.Mutex

	writes  []*v1.WriteRelationshipsRequest
	deletes []*v1.DeleteRelationshipsRequest
}

func (w *fakeWriter) WriteRelationships(_ context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, req)
	return &v1.WriteRelationshipsResponse{}, nil
}

func (w *fakeWriter) DeleteRelationships(_ context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deletes = append(w.deletes, req)
	return &v1.DeleteRelationshipsResponse{}, nil
}

// fakeReadStream is the minimal v1.PermissionsService_ReadRelationshipsClient
// fixture — same shape as pkg/controllers/guardian/bootstrap_drift_test.go's
// fakeReadStream: serves canned responses off a slice, then io.EOF.
// grpc.ClientStream is embedded (nil) purely to satisfy the rest of the
// interface's method set, which relsync's Pass never calls.
type fakeReadStream struct {
	grpc.ClientStream
	resps []*v1.ReadRelationshipsResponse
	idx   int
}

func (s *fakeReadStream) Recv() (*v1.ReadRelationshipsResponse, error) {
	if s.idx >= len(s.resps) {
		return nil, io.EOF
	}
	r := s.resps[s.idx]
	s.idx++
	return r, nil
}

// fakeReader answers relsync's two distinct read shapes:
//
//   - a narrow #relhash lookup (OptionalRelation == "relhash"), answered
//     from hashes, keyed "resourceType:scopeID" — this is the cheap check
//     that always happens, and it never counts toward scopeReads.
//   - a full per-scope or reap-scan read (any other filter), answered by
//     filtering owned — and, when the filter names one resource id, that
//     id is recorded into scopeReads. This is the read
//     TestPass_UnchangedScopeIsNotRead asserts is ABSENT for an unchanged
//     scope.
type fakeReader struct {
	mu sync.Mutex

	owned      []spicedb.Tuple
	hashes     map[string]string // "resourceType:scopeID" -> current sentinel value
	scopeReads []string          // "resourceType:scopeID" for each FULL per-scope read
}

func (r *fakeReader) ReadRelationships(_ context.Context, req *v1.ReadRelationshipsRequest) (v1.PermissionsService_ReadRelationshipsClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	filter := req.GetRelationshipFilter()

	if filter.GetOptionalRelation() == "relhash" {
		key := filter.GetResourceType() + ":" + filter.GetOptionalResourceId()
		if h, ok := r.hashes[key]; ok {
			return &fakeReadStream{resps: []*v1.ReadRelationshipsResponse{
				sentinelResponse(filter.GetResourceType(), filter.GetOptionalResourceId(), h),
			}}, nil
		}
		return &fakeReadStream{}, nil
	}

	if filter.GetOptionalResourceId() != "" {
		r.scopeReads = append(r.scopeReads, filter.GetResourceType()+":"+filter.GetOptionalResourceId())
	}

	var resps []*v1.ReadRelationshipsResponse
	for _, t := range r.owned {
		if t.ResourceType != filter.GetResourceType() {
			continue
		}
		if filter.GetOptionalResourceId() != "" && t.ResourceID != filter.GetOptionalResourceId() {
			continue
		}
		if filter.GetOptionalRelation() != "" && t.Relation != filter.GetOptionalRelation() {
			continue
		}
		resps = append(resps, tupleResponse(t))
	}
	return &fakeReadStream{resps: resps}, nil
}

func tupleResponse(t spicedb.Tuple) *v1.ReadRelationshipsResponse {
	subj := &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: t.SubjectType, ObjectId: t.SubjectID}}
	if t.SubjectRelation != "" {
		subj.OptionalRelation = t.SubjectRelation
	}
	return &v1.ReadRelationshipsResponse{
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: t.ResourceType, ObjectId: t.ResourceID},
			Relation: t.Relation,
			Subject:  subj,
		},
	}
}

func sentinelResponse(resourceType, resourceID, hash string) *v1.ReadRelationshipsResponse {
	return &v1.ReadRelationshipsResponse{
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID},
			Relation: "relhash",
			Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "string", ObjectId: hash}},
		},
	}
}

// tup builds a slack_channel:<scopeID>#member@slack_user:<subjectID> tuple
// — the one shape every test below needs, matching the Scope{ResourceType:
// "slack_channel"} fixtures used throughout.
func tup(scopeID, subjectID string) spicedb.Tuple {
	return spicedb.Tuple{
		ResourceType: "slack_channel",
		ResourceID:   scopeID,
		Relation:     "member",
		SubjectType:  "slack_user",
		SubjectID:    subjectID,
	}
}

// scopeList builds []relsync.Scope from bare IDs, all sharing ResourceType
// "slack_channel" — the shape TestPass_ConvergesAcrossRepeatedPassesWithASmallBudget's
// multi-page fixture needs.
func scopeList(ids ...string) []relsync.Scope {
	out := make([]relsync.Scope, len(ids))
	for i, id := range ids {
		out[i] = relsync.Scope{ID: relsync.ScopeID(id), ResourceType: "slack_channel"}
	}
	return out
}

// findWriteForResource returns the single WriteRelationships call among
// writes that touches (resourceType, resourceID) in any of its updates —
// failing the test if none (or, implicitly, if the caller expected
// exactly one write per scope and got none).
func findWriteForResource(t *testing.T, writes []*v1.WriteRelationshipsRequest, resourceType, resourceID string) *v1.WriteRelationshipsRequest {
	t.Helper()
	for _, w := range writes {
		for _, u := range w.GetUpdates() {
			res := u.GetRelationship().GetResource()
			if res.GetObjectType() == resourceType && res.GetObjectId() == resourceID {
				return w
			}
		}
	}
	require.Fail(t, fmt.Sprintf("no WriteRelationships call found touching %s:%s", resourceType, resourceID))
	return nil
}

// writeTouches reports whether w contains a TOUCH update for exactly
// (resourceType, resourceID, relation).
func writeTouches(w *v1.WriteRelationshipsRequest, resourceType, resourceID, relation string) bool {
	for _, u := range w.GetUpdates() {
		if u.GetOperation() != v1.RelationshipUpdate_OPERATION_TOUCH {
			continue
		}
		res := u.GetRelationship().GetResource()
		if res.GetObjectType() == resourceType && res.GetObjectId() == resourceID && u.GetRelationship().GetRelation() == relation {
			return true
		}
	}
	return false
}

// A truncated enumeration is non-empty, well-formed and short. Reaping
// against it deletes every scope it never reached — the case an emptiness
// check cannot catch. Assert on the WRITE CALLS: a test checking only the
// result would pass if a delete were added later.
func TestPass_PartialEnumerationNeverReaps(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}},
			Complete: false, // stopped early
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": {tup("C1", "alice")}},
	}
	w := &fakeWriter{}
	// SpiceDB already holds a scope the truncated list never mentions.
	r := &fakeReader{owned: []spicedb.Tuple{tup("C9", "bob")}}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})

	require.NoError(t, err)
	assert.False(t, res.EnumComplete)
	assert.Empty(t, w.deletes, "a truncated enumeration must delete NOTHING")
	assert.Equal(t, 0, res.ReapedScopes)
	assert.NotEmpty(t, w.writes, "but the scopes it did see are still synced")
}

// The counterpart: a complete enumeration reaps a scope SpiceDB holds and
// upstream no longer lists.
func TestPass_CompleteEnumerationReapsAnAbsentScope(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": {tup("C1", "alice")}},
	}
	w := &fakeWriter{}
	// SpiceDB holds C9 too, but the (complete) enumeration never mentions
	// it — upstream deleted the channel entirely.
	r := &fakeReader{owned: []spicedb.Tuple{tup("C9", "bob")}}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})

	require.NoError(t, err)
	assert.True(t, res.EnumComplete)
	require.Len(t, w.deletes, 1, "the orphaned scope must be swept by a single broad delete")
	assert.Equal(t, "slack_channel", w.deletes[0].GetRelationshipFilter().GetResourceType())
	assert.Equal(t, "C9", w.deletes[0].GetRelationshipFilter().GetOptionalResourceId())
	assert.Empty(t, w.deletes[0].GetRelationshipFilter().GetOptionalRelation(),
		"the reap sweeps every relation on the orphaned resource, sentinel included")
	assert.Equal(t, 1, res.ReapedScopes)
}

// A scope is never written half-fetched: the per-scope prune deletes
// whatever is absent, so a partial fetch would delete the members it had
// not reached.
func TestPass_FetchErrorWritesAndPrunesNothingForThatScope(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages:     []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}}, Complete: true}},
		fetchErrs: map[relsync.ScopeID]error{"C1": errors.New("page 2 of 3 failed")},
	}
	w := &fakeWriter{}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: &fakeReader{}})

	require.NoError(t, err, "one scope failing is not a pass failure")
	assert.Empty(t, w.writes)
	assert.Empty(t, w.deletes)
	assert.Len(t, res.ScopeErrors, 1)
}

// ErrScopeGone is distinct from an empty result: gone means prune the
// scope, empty means the scope exists with no members.
func TestPass_ScopeGoneIsPrunedButEmptyScopeIsNot(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes: []relsync.Scope{
				{ID: "C1", ResourceType: "slack_channel"},
				{ID: "C2", ResourceType: "slack_channel"},
			},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{
			"C2": {}, // C2 exists upstream with zero members — legitimately empty
		},
		fetchErrs: map[relsync.ScopeID]error{
			"C1": relsync.ErrScopeGone,
		},
	}
	w := &fakeWriter{}
	r := &fakeReader{owned: []spicedb.Tuple{
		tup("C1", "alice"), // C1's stale member, still present before this pass
		tup("C2", "bob"),   // C2's stale member — upstream now reports empty
	}}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})

	require.NoError(t, err)
	assert.Equal(t, 1, res.ReapedScopes, "only the GONE scope counts as a whole-scope reap")

	// C1 is GONE: its entire resource object is swept in one broad delete,
	// never diffed as ordinary content.
	require.Len(t, w.deletes, 1)
	assert.Equal(t, "slack_channel", w.deletes[0].GetRelationshipFilter().GetResourceType())
	assert.Equal(t, "C1", w.deletes[0].GetRelationshipFilter().GetOptionalResourceId())
	assert.Empty(t, w.deletes[0].GetRelationshipFilter().GetOptionalRelation())

	// C2 is EMPTY, not gone: its stale member is removed by an ordinary
	// scoped write (TOUCH sentinel + DELETE member), never by
	// DeleteRelationships.
	c2 := findWriteForResource(t, w.writes, "slack_channel", "C2")
	var sawMemberDelete, sawSentinelTouch bool
	for _, u := range c2.GetUpdates() {
		res := u.GetRelationship().GetResource()
		if res.GetObjectId() != "C2" {
			continue
		}
		switch u.GetRelationship().GetRelation() {
		case "member":
			assert.Equal(t, v1.RelationshipUpdate_OPERATION_DELETE, u.GetOperation())
			sawMemberDelete = true
		case "relhash":
			assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, u.GetOperation())
			sawSentinelTouch = true
		}
	}
	assert.True(t, sawMemberDelete, "C2's stale member must be pruned via the ordinary write path")
	assert.True(t, sawSentinelTouch, "C2 still gets a fresh (empty-set) sentinel — it exists, just with no members")
}

// Resume COMPARES. A scope deleted between passes — the exact case orphan
// reaping exists for — must not strand the cursor or skip its neighbour.
func TestPass_ResumesAfterAScopeThatNoLongerExists(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}, {ID: "C3", ResourceType: "slack_channel"}}, // C2 deleted
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C3": {tup("C3", "carol")}},
	}
	w := &fakeWriter{}

	res, err := relsync.Pass(ctx, relsync.PassInput{
		Kind: k, Writer: w, Reader: &fakeReader{}, ResumeAfter: "C2",
	})

	require.NoError(t, err)
	assert.Equal(t, 1, res.Processed, "C3 sorts after the deleted C2 and must still be processed")
	assert.Equal(t, []relsync.ScopeID{"C3"}, k.fetchCalls, "specifically C3, not C1 (already before the resume point)")
}

// The hash short-circuit, asserted by the ABSENCE of a read — not by the
// absence of a change. A result-only assertion would pass while the code
// read and diffed every scope anyway.
func TestPass_UnchangedScopeIsNotRead(t *testing.T) {
	ctx := context.Background()
	members := []spicedb.Tuple{tup("C1", "alice")}
	k := &fakePassKind{
		pages:   []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}}, Complete: true}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": members},
	}
	r := &fakeReader{hashes: map[string]string{"slack_channel:C1": relsync.HashTuples(members)}}
	w := &fakeWriter{}

	_, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})

	require.NoError(t, err)
	assert.Empty(t, r.scopeReads, "an unchanged scope must not be read at all")
	assert.Empty(t, w.writes)
}

// The budget stops the pass cleanly and reports where, so the next pass
// resumes rather than restarting.
func TestPass_BudgetStopsAndReportsResumePoint(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages: []relsync.ScopePage{
			{
				Scopes: []relsync.Scope{
					{ID: "C1", ResourceType: "slack_channel"},
					{ID: "C2", ResourceType: "slack_channel"},
				},
				Next:     relsync.Cursor{Kind: "fakepass", Token: "1"}, // fakePassKind decodes Token as a page index
				Complete: false,                                        // NOT the final page -- more still exist upstream, so this is not "the enumeration finished"
			},
			{
				Scopes:   []relsync.Scope{{ID: "C3", ResourceType: "slack_channel"}},
				Complete: true,
			},
		},
		members: map[relsync.ScopeID][]spicedb.Tuple{
			"C1": {tup("C1", "alice")},
			"C2": {tup("C2", "bob")},
			"C3": {tup("C3", "carol")},
		},
	}
	w := &fakeWriter{}
	// A scope the budget never reached this pass -- present so the
	// deletes-empty assertion below is falsifiable: if the enumComplete
	// gate on reaping were ever dropped, C9 would be (wrongly) swept.
	r := &fakeReader{owned: []spicedb.Tuple{tup("C9", "carol")}}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r, MaxScopes: 2})

	require.NoError(t, err)
	assert.Equal(t, 1, k.listCalls, "the budget must stop enumeration before a second ListScopes call")
	assert.False(t, res.EnumComplete, "a budget-truncated enumeration is exactly as untrustworthy for reaping as an upstream-truncated one")
	assert.False(t, res.CycleComplete)
	assert.Equal(t, relsync.ScopeID("C2"), res.ResumeAfter, "the next pass must resume after the last scope this one actually processed")
	assert.Empty(t, w.deletes, "a budget-truncated pass must reap nothing, same as any other incomplete enumeration")
}

// Ruling 4 (progress.md, carried into this task's dispatch): the per-scope
// prune is bounded to tuples whose RESOURCE is the scope's own object.
// Fetching a channel can yield a tuple on ANOTHER resource entirely — an
// identity edge that is workspace-wide, not channel-scoped. An unbounded
// per-scope diff would see that edge "missing" from a channel that
// stopped mentioning it and delete it out from under every OTHER channel
// that still needs it — each channel fighting the last, membership
// flapping on every pass.
//
// Fix round 2 (IMPORTANT 1): the reviewer proved the ORIGINAL version of
// this test didn't test the bound at all — deleting
// `OptionalResourceId: string(scope.ID)` from readScopeTuples left the
// whole package green, this test included, because the shared tuple's
// different RESOURCE TYPE ("slack_user" vs "slack_channel") is excluded by
// RelationshipFilter.ResourceType on its own, and the fake reader cannot
// return a cross-TYPE tuple regardless of the resource-id filter. Two
// fixes: assert on the recorded read filters directly (r.scopeReads),
// which fails the moment the per-object bound goes even though the
// cross-type case still passes; and add a second SAME-type scope (C2, a
// slack_channel like C1) with its own stale member in owned, which an
// unbounded read WOULD catch and wrongly prune through C1's write.
func TestPass_SharedResourceTupleAcrossScopesIsNeverPruned(t *testing.T) {
	ctx := context.Background()
	sharedIdentity := spicedb.Tuple{
		ResourceType: "slack_user", ResourceID: "U1", Relation: "user",
		SubjectType: "user", SubjectID: "alice",
	}
	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes: []relsync.Scope{
				{ID: "C1", ResourceType: "slack_channel"},
				{ID: "C2", ResourceType: "slack_channel"},
			},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{
			// C1 no longer has U1 as a member — its fetch this pass mentions
			// nothing on slack_user at all.
			"C1": {tup("C1", "bob")},
			// C2 still has U1, alongside its own current channel membership
			// (which no longer includes carolold below).
			"C2": {tup("C2", "carol"), sharedIdentity},
		},
	}
	w := &fakeWriter{}
	r := &fakeReader{owned: []spicedb.Tuple{
		tup("C1", "u1old"),    // C1's stale member, about to be replaced by bob
		tup("C2", "carolold"), // C2's OWN stale member — same type as C1, different resource id
		sharedIdentity,        // already exists, written by some earlier pass
	}}

	_, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})
	require.NoError(t, err)

	// The read bound itself: each scope's read is filtered to its OWN
	// resource id, nothing broader. Removing OptionalResourceId from
	// readScopeTuples makes this fail immediately (empty, since the fake
	// only records scopeReads when a resource id is present) even though
	// every other assertion below still passes.
	assert.Equal(t, []string{"slack_channel:C1", "slack_channel:C2"}, r.scopeReads)

	// C1's write must touch only its own resource — never slack_user:U1,
	// AND never slack_channel:C2, even though C1's fetch never mentions
	// carolold and an unbounded read would see it as "missing" from C1's
	// own diff.
	c1 := findWriteForResource(t, w.writes, "slack_channel", "C1")
	for _, u := range c1.GetUpdates() {
		res := u.GetRelationship().GetResource()
		assert.NotEqual(t, "slack_user", res.GetObjectType(), "C1's write must never touch another scope's resource TYPE")
		assert.NotEqual(t, "C2", res.GetObjectId(), "C1's write must never touch another scope's resource ID, same type or not")
	}
	assert.Empty(t, w.deletes, "no whole-scope reap is in play here; every prune must ride the ordinary per-scope write")

	// C2's write is where BOTH the shared edge (additive, never owned by
	// or deletable through C2 alone) and C2's OWN stale member (properly
	// prunable, because it's C2's own resource) legitimately show up.
	c2 := findWriteForResource(t, w.writes, "slack_channel", "C2")
	assert.True(t, writeTouches(c2, "slack_user", "U1", "user"),
		"C2 still needs the shared identity edge and must (re)assert it")
	var sawOwnMemberDelete bool
	for _, u := range c2.GetUpdates() {
		res := u.GetRelationship().GetResource()
		if res.GetObjectType() == "slack_channel" && res.GetObjectId() == "C2" &&
			u.GetRelationship().GetRelation() == "member" && u.GetOperation() == v1.RelationshipUpdate_OPERATION_DELETE {
			sawOwnMemberDelete = true
		}
	}
	assert.True(t, sawOwnMemberDelete, "C2's OWN stale member must still be prunable through C2's own write")
}

// A source with more scopes than the budget must still converge across
// repeated passes: the resume filter and the enumeration budget have to
// meet in the middle, not both start from the beginning every time.
// TestPass_BudgetStopsAndReportsResumePoint only exercises a single pass
// and cannot catch a deadlock that only appears once ResumeAfter has moved
// — this test drives Pass repeatedly, threading each result's ResumeAfter
// into the next call's PassInput, exactly as the controller would across
// reconciles.
//
// 10 scopes across 3 pages, budget 3: pass 1 collects (and processes)
// C01-C03; pass 2's enumeration must page PAST C01-C03 (already before the
// new ResumeAfter) to collect C04-C06 before the budget is satisfied; pass
// 3 reaches the true end of upstream (C07-C10, overshooting the budget
// since it's the last page) and completes the cycle.
func TestPass_ConvergesAcrossRepeatedPassesWithASmallBudget(t *testing.T) {
	ctx := context.Background()
	allScopes := []string{"C01", "C02", "C03", "C04", "C05", "C06", "C07", "C08", "C09", "C10"}

	k := &fakePassKind{
		pages: []relsync.ScopePage{
			{ // page 0 -- NOT the final page: more still exist upstream
				Scopes:   scopeList("C01", "C02", "C03"),
				Next:     relsync.Cursor{Kind: "fakepass", Token: "1"},
				Complete: false,
			},
			{ // page 1 -- NOT the final page either
				Scopes:   scopeList("C04", "C05", "C06"),
				Next:     relsync.Cursor{Kind: "fakepass", Token: "2"},
				Complete: false,
			},
			{ // page 2 — the last page: Next is left as the zero Cursor
				Scopes:   scopeList("C07", "C08", "C09", "C10"),
				Complete: true,
			},
		},
		members: map[relsync.ScopeID][]spicedb.Tuple{},
	}
	for _, id := range allScopes {
		k.members[relsync.ScopeID(id)] = []spicedb.Tuple{tup(id, "user-"+id)}
	}
	w := &fakeWriter{}
	r := &fakeReader{}

	const maxScopes = 3
	const maxPasses = 10 // generous; convergence should take 3 passes

	var resumeAfter relsync.ScopeID
	converged := false
	for pass := 0; pass < maxPasses; pass++ {
		res, err := relsync.Pass(ctx, relsync.PassInput{
			Kind: k, Writer: w, Reader: r,
			ResumeAfter: resumeAfter, MaxScopes: maxScopes,
		})
		require.NoError(t, err)
		require.Empty(t, res.ScopeErrors, "pass %d", pass)
		resumeAfter = res.ResumeAfter
		if res.CycleComplete {
			converged = true
			break
		}
	}

	require.True(t, converged,
		"CycleComplete was never reported within %d passes — the budget and the resume cursor never met", maxPasses)

	fetchCount := map[relsync.ScopeID]int{}
	for _, id := range k.fetchCalls {
		fetchCount[id]++
	}
	for _, id := range allScopes {
		assert.Equal(t, 1, fetchCount[relsync.ScopeID(id)], "scope %s must be fetched exactly once across the whole cycle", id)
	}
}

// CRITICAL (fix round 2): a relationship is keyed by (resource, relation,
// subject), and the hash rides as the subject id — so TOUCHing a new hash
// creates a NEW tuple, never updates the old one. Before this fix,
// diffScope emitted only the new sentinel's TOUCH, and readScopeTuples
// deliberately excludes #relhash from owned, so nothing ever computed a
// DELETE for the old one: the scope accumulated one #relhash tuple per
// content state it had ever held, "the current hash" became stream-order
// dependent (content A -> B -> A left a stored hash matching a fresh A
// while SpiceDB still held B's tuples, so the scope stopped converging),
// and the MUST_MATCH precondition ended up satisfiable by a stale
// sentinel from the second write on, guarding nothing.
//
// This asserts the single write for a changed scope carries all three:
// TOUCH(new hash), DELETE(old hash), and a MUST_MATCH precondition naming
// the old hash.
func TestPass_ChangedSentinelDeletesTheOldOne(t *testing.T) {
	ctx := context.Background()
	oldMembers := []spicedb.Tuple{tup("C1", "alice")}
	newMembers := []spicedb.Tuple{tup("C1", "alice"), tup("C1", "bob")}
	oldHash := relsync.HashTuples(oldMembers)
	newHash := relsync.HashTuples(newMembers)
	require.NotEqual(t, oldHash, newHash, "fixture sanity: the content must actually differ")

	k := &fakePassKind{
		pages:   []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}}, Complete: true}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": newMembers},
	}
	r := &fakeReader{
		owned:  append([]spicedb.Tuple{}, oldMembers...),
		hashes: map[string]string{"slack_channel:C1": oldHash},
	}
	w := &fakeWriter{}

	_, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})
	require.NoError(t, err)

	c1 := findWriteForResource(t, w.writes, "slack_channel", "C1")

	var sawTouchNew, sawDeleteOld bool
	for _, u := range c1.GetUpdates() {
		rel := u.GetRelationship()
		res := rel.GetResource()
		if res.GetObjectType() != "slack_channel" || res.GetObjectId() != "C1" || rel.GetRelation() != "relhash" {
			continue
		}
		subjID := rel.GetSubject().GetObject().GetObjectId()
		switch {
		case u.GetOperation() == v1.RelationshipUpdate_OPERATION_TOUCH && subjID == newHash:
			sawTouchNew = true
		case u.GetOperation() == v1.RelationshipUpdate_OPERATION_DELETE && subjID == oldHash:
			sawDeleteOld = true
		}
	}
	assert.True(t, sawTouchNew, "the new sentinel must be TOUCHed")
	assert.True(t, sawDeleteOld, "the OLD sentinel must be DELETEd in the SAME write, or it lingers forever")

	require.Len(t, c1.GetOptionalPreconditions(), 1)
	precond := c1.GetOptionalPreconditions()[0]
	assert.Equal(t, v1.Precondition_OPERATION_MUST_MATCH, precond.GetOperation())
	assert.Equal(t, "relhash", precond.GetFilter().GetOptionalRelation())
	assert.Equal(t, oldHash, precond.GetFilter().GetOptionalSubjectFilter().GetOptionalSubjectId())
}

// deletedTuple scans every WriteRelationships call recorded in writes for a
// DELETE-operation update matching (resourceType, resourceID, subjectID) —
// the shape the tuple-granularity cross-resource reap now uses
// (reapAbsentCrossResourceTuples issues WriteRelationships, never
// DeleteRelationships, precisely so it can name a subject and not just a
// resource id). subjectID == "" matches any subject, for callers that only
// care whether ANY of a resource id's tuples were deleted.
func deletedTuple(writes []*v1.WriteRelationshipsRequest, resourceType, resourceID, subjectID string) bool {
	for _, w := range writes {
		for _, u := range w.GetUpdates() {
			if u.GetOperation() != v1.RelationshipUpdate_OPERATION_DELETE {
				continue
			}
			rel := u.GetRelationship()
			res := rel.GetResource()
			if res.GetObjectType() != resourceType || res.GetObjectId() != resourceID {
				continue
			}
			if subjectID != "" && rel.GetSubject().GetObject().GetObjectId() != subjectID {
				continue
			}
			return true
		}
	}
	return false
}

// IMPORTANT 2 (fix round 2): Ruling 4 bounds the per-scope prune to the
// scope's own resource, which is correct, but granted that a cross-resource
// tuple (a workspace-wide identity edge) is "reaped on the full pass
// against the union of what every scope produced." Nothing built that
// union — processScope discarded a fetch's cross-resource tuples entirely,
// and reapAbsentScopes only ever scanned types that appear as a
// Scope.ResourceType. An identity edge no scope's fetch mentions any more
// lingered FOREVER, not "until the next full pass" as granted.
//
// U1 is still asserted by C1 and must survive; U2 is asserted by nothing
// and must be reaped, on a pass that fetches every scope.
//
// Whole-branch review, Critical 2: the reap this exercises now goes
// through WriteRelationships (a precise per-tuple DELETE), never the
// broad, resource-id-only DeleteRelationships sweep it used before — see
// reapAbsentCrossResourceTuples's own doc for why. Assert on w.writes, not
// w.deletes.
func TestPass_CrossResourceOrphanReapedAgainstTheUnionOfEveryScope(t *testing.T) {
	ctx := context.Background()
	stillNeeded := spicedb.Tuple{
		ResourceType: "slack_user", ResourceID: "U1", Relation: "user",
		SubjectType: "user", SubjectID: "alice",
	}
	orphaned := spicedb.Tuple{
		ResourceType: "slack_user", ResourceID: "U2", Relation: "user",
		SubjectType: "user", SubjectID: "bob",
	}
	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes: []relsync.Scope{
				{ID: "C1", ResourceType: "slack_channel"},
				{ID: "C2", ResourceType: "slack_channel"},
			},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{
			"C1": {tup("C1", "u1"), stillNeeded}, // C1 still needs U1
			"C2": {tup("C2", "carol")},           // C2's member (U2) left; not mentioned at all any more
		},
	}
	w := &fakeWriter{}
	r := &fakeReader{owned: []spicedb.Tuple{stillNeeded, orphaned}}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})
	require.NoError(t, err)

	assert.True(t, deletedTuple(w.writes, "slack_user", "U2", "bob"),
		"U2 is referenced by no scope any more and must be reaped once the pass processes everything")
	assert.False(t, deletedTuple(w.writes, "slack_user", "U1", ""),
		"U1 is still referenced by C1 and must never be reaped")
	assert.Empty(t, w.deletes, "the cross-resource reap must never use a broad DeleteRelationships sweep")
	assert.GreaterOrEqual(t, res.Pruned, 1, "a cross-resource deletion is tuple-level, so it counts as Pruned, not ReapedScopes")
}

// Whole-branch review, Critical 2 — the case the review proved by probe: a
// member's identity edge changes SUBJECT (a Slack user's email changing,
// or any other re-canonicalization) between two passes, while the
// membership itself is unchanged. Before this fix, the resource id
// (slack_user:U1) never left the cross-resource union — SOME tuple on it
// was still asserted — so the reap left it alone entirely and the STALE
// subject (the old identity) kept granting whatever it always granted,
// forever. That is the fail-OPEN direction the whole-branch review named:
// audience_resolver.go hands an inflated membership to the info-leakage
// gate.
//
// Fixed: the new tuple is TOUCHed (via the ordinary per-scope diff, since
// it rides on C1's fetch) and the OLD tuple is DELETEd by the
// cross-resource reap — same resource id, different Key(), so the id
// surviving the union no longer protects the stale tuple.
func TestPass_ChangedCrossResourceSubjectDeletesTheOldEdge(t *testing.T) {
	ctx := context.Background()
	channelMember := spicedb.Tuple{
		ResourceType: "slack_channel", ResourceID: "C1", Relation: "member",
		SubjectType: "slack_user", SubjectID: "U1", SubjectRelation: "user",
	}
	oldIdentity := spicedb.Tuple{
		ResourceType: "slack_user", ResourceID: "U1", Relation: "user",
		SubjectType: "user", SubjectID: "alice-old",
	}
	newIdentity := spicedb.Tuple{
		ResourceType: "slack_user", ResourceID: "U1", Relation: "user",
		SubjectType: "user", SubjectID: "alice-new",
	}
	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{
			// Same membership as before; the identity edge's SUBJECT changed.
			"C1": {channelMember, newIdentity},
		},
	}
	w := &fakeWriter{}
	r := &fakeReader{owned: []spicedb.Tuple{channelMember, oldIdentity}}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})
	require.NoError(t, err)
	require.Empty(t, res.ScopeErrors)

	assert.True(t, deletedTuple(w.writes, "slack_user", "U1", "alice-old"),
		"the stale identity edge must be deleted once the fetch stops asserting it, even though slack_user:U1 itself is still asserted (by the fresh edge)")
	assert.False(t, deletedTuple(w.writes, "slack_user", "U1", "alice-new"),
		"the fresh identity edge must never be deleted")
	c1 := findWriteForResource(t, w.writes, "slack_channel", "C1")
	assert.True(t, writeTouches(c1, "slack_user", "U1", "user"),
		"the fresh identity edge must be (re)asserted via the ordinary per-scope diff")
	assert.Empty(t, w.deletes, "the cross-resource reap must go through WriteRelationships, never a broad DeleteRelationships sweep")
	assert.GreaterOrEqual(t, res.Pruned, 1)
}

// BLOCKER (fix round 3): pass COVERAGE (toProcess/scopes matching in
// length) is not the same thing as FETCH coverage. C1's fetch fails here
// (a stand-in for a transient 429/timeout, not ErrScopeGone), so
// processScope returns before ever reaching recordExtraResources — C1
// contributes NOTHING to extraResources this pass, even though every
// scope was still enumerated and "processed" in the sense of being
// visited. Before this fix, the union gate checked only
// len(toProcess)==len(scopes), which stays true regardless of a fetch
// failure, so U1 (only ever asserted by C1) looked orphaned and got swept
// by the cross-resource reap — a live identity edge destroyed by a single
// transient upstream error, in the unbounded default configuration where
// the union is supposed to be trustworthy.
func TestPass_FailedFetchNeverDefeatsTheCrossResourceUnion(t *testing.T) {
	ctx := context.Background()
	// U1 is asserted ONLY by C1 (about to fail). U2 is asserted by C2 (which
	// succeeds) -- this is what makes "slack_user" a type the union even
	// bothers to scan; without a SURVIVING reference to the type, nothing
	// this test could observe would distinguish the fixed code from the
	// bug (extraResources would be empty either way, and an empty map is
	// never scanned at all, regardless of the gate). U1 is the one only a
	// FAILED scope ever asserted, and is what a broken gate wrongly treats
	// as orphaned.
	stillNeededByC1 := spicedb.Tuple{
		ResourceType: "slack_user", ResourceID: "U1", Relation: "user",
		SubjectType: "user", SubjectID: "alice",
	}
	stillNeededByC2 := spicedb.Tuple{
		ResourceType: "slack_user", ResourceID: "U2", Relation: "user",
		SubjectType: "user", SubjectID: "bob",
	}
	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes: []relsync.Scope{
				{ID: "C1", ResourceType: "slack_channel"},
				{ID: "C2", ResourceType: "slack_channel"},
			},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{
			"C2": {tup("C2", "carol"), stillNeededByC2},
		},
		fetchErrs: map[relsync.ScopeID]error{
			// C1's fetch fails outright -- NOT gone, just unreachable this
			// pass. Whether C1 still needs U1 is genuinely unknown.
			"C1": errors.New("429 rate limited"),
		},
	}
	w := &fakeWriter{}
	r := &fakeReader{owned: []spicedb.Tuple{stillNeededByC1, stillNeededByC2}}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})
	require.NoError(t, err)

	require.Len(t, res.ScopeErrors, 1, "C1's fetch failure must still be reported")
	assert.Equal(t, relsync.ScopeID("C1"), res.ScopeErrors[0].Scope)

	// The whole cross-resource reap must sit out this pass -- not just
	// spare U1 while still reaping U2. Fetch coverage is incomplete, so
	// the union built from what succeeded is not trustworthy for ANY id
	// on a type it touches, not only the one a failed scope would have
	// asserted. Checked at TUPLE granularity (deletedTuple, over w.writes)
	// since the whole-branch review's Critical 2 fix moved the cross-
	// resource reap off DeleteRelationships entirely — w.deletes staying
	// empty no longer, by itself, proves this gate held.
	assert.False(t, deletedTuple(w.writes, "slack_user", "U1", ""),
		"a scope's fetch failure must close the cross-resource reap entirely, not just spare the id that scope would have asserted")
	assert.False(t, deletedTuple(w.writes, "slack_user", "U2", ""),
		"U2's own successful scope must not make the reap trust a union the pass overall did not earn")
	assert.Empty(t, w.deletes)
}

// BLOCKER (final whole-branch review): the cross-resource reap deleted
// every tuple on a cross-resource TYPE whose key was absent from the
// union, without ever asking whether the source was allowed to write that
// relation in the first place. The union only ever holds relations the
// source's own fetches assert, so every OTHER relation on the type — a
// human's approval, a live slot grant, anything a different writer owns —
// fell out of it by construction and was deleted.
//
// GitHub is where it surfaced (github_repo_url carries the sync's #repo
// bridge edge alongside #owner and slot_grant_*), but the defect is the
// engine's: Slack's slack_user happened to have exactly one relation and
// one writer, so nothing else was ever there to destroy.
//
// The fixture is the GitHub shape deliberately, because that is the shape
// that proves it: one claimed relation on the type and two unclaimed ones,
// all three absent from the union.
func TestPass_CrossResourceReapNeverDeletesARelationTheSourceDoesNotClaim(t *testing.T) {
	ctx := context.Background()
	// Claimed, still asserted by the fetch: must survive.
	liveBridge := spicedb.Tuple{
		ResourceType: "github_repo_url", ResourceID: "urlA", Relation: "repo",
		SubjectType: "github_repo", SubjectID: "R1",
	}
	// Claimed, asserted by nothing any more: the reap's actual job.
	staleBridge := spicedb.Tuple{
		ResourceType: "github_repo_url", ResourceID: "urlB", Relation: "repo",
		SubjectType: "github_repo", SubjectID: "R-GONE",
	}
	// Unclaimed — a human's approval, written by the approval flow, on the
	// very same object the stale bridge edge sits on.
	humanApproval := spicedb.Tuple{
		ResourceType: "github_repo_url", ResourceID: "urlB", Relation: "owner",
		SubjectType: "user", SubjectID: "alice",
	}
	// Unclaimed — a live slot grant for a slotted class.
	slotGrant := spicedb.Tuple{
		ResourceType: "github_repo_url", ResourceID: "urlB", Relation: "slot_grant_write",
		SubjectType: "agentsession", SubjectID: "sess-1",
	}
	k := &fakePassKind{
		claims: []string{
			"github_org#member",
			"github_org#relhash",
			"github_repo_url#repo",
		},
		pages: []relsync.ScopePage{{
			Scopes:   []relsync.Scope{{ID: "O1", ResourceType: "github_org"}},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{
			"O1": {
				{
					ResourceType: "github_org", ResourceID: "O1", Relation: "member",
					SubjectType: "user", SubjectID: "alice",
				},
				liveBridge,
			},
		},
	}
	w := &fakeWriter{}
	r := &fakeReader{owned: []spicedb.Tuple{liveBridge, staleBridge, humanApproval, slotGrant}}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})
	require.NoError(t, err)
	require.Empty(t, res.ScopeErrors)

	assert.True(t, deletedTuple(w.writes, "github_repo_url", "urlB", "R-GONE"),
		"a CLAIMED relation absent from the union is exactly what the cross-resource reap is for; bounding it by claims must not disarm it")
	assert.False(t, deletedTuple(w.writes, "github_repo_url", "urlA", ""),
		"the bridge edge the fetch still asserts must survive")
	assert.False(t, deletedTuple(w.writes, "github_repo_url", "urlB", "alice"),
		"#owner is a human's approval this source never writes: deleting it revokes an approval nobody withdrew")
	assert.False(t, deletedTuple(w.writes, "github_repo_url", "urlB", "sess-1"),
		"slot_grant_write is a live grant this source never writes: deleting it cuts a running session off mid-flight")
	assert.Equal(t, 1, res.Pruned, "exactly one tuple — the stale claimed one — may be pruned")
}

// MAJOR (fix round 3): the two "known" maps used to be merged into one,
// which meant a stray tuple a fetch asserts against ANOTHER object of the
// SAME resource type as a real scope (not just a different type) could
// get folded into the very map the scope-level reap uses to decide what's
// still known -- protecting an unenumerated object from that reap on a
// pass where the merge ran, while a pass where it didn't (a
// budget-bounded partial pass) reaped it correctly. Same real SpiceDB
// state, different outcome depending on an unrelated config knob. C99 is
// never enumerated as a scope in either subtest below; both must reap it.
func TestPass_SameTypeStrayReferenceNeverProtectsAnUnenumeratedScope(t *testing.T) {
	stray := spicedb.Tuple{
		ResourceType: "slack_channel", ResourceID: "C99", Relation: "member",
		SubjectType: "slack_user", SubjectID: "zed",
	}
	run := func(t *testing.T, maxScopes int) {
		ctx := context.Background()
		k := &fakePassKind{
			pages: []relsync.ScopePage{{
				Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}},
				Complete: true,
			}},
			members: map[relsync.ScopeID][]spicedb.Tuple{
				// C1's own fetch happens to also mention a tuple on C99's
				// object -- Ruling 4 lets a fetch return tuples on ANY
				// other resource, same type or not.
				"C1": {tup("C1", "alice"), stray},
			},
		}
		w := &fakeWriter{}
		// SpiceDB holds C99 -- a real object, never enumerated as a scope.
		r := &fakeReader{owned: []spicedb.Tuple{stray}}

		_, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r, MaxScopes: maxScopes})
		require.NoError(t, err)

		var reapedC99 bool
		for _, d := range w.deletes {
			f := d.GetRelationshipFilter()
			if f.GetResourceType() == "slack_channel" && f.GetOptionalResourceId() == "C99" {
				reapedC99 = true
			}
		}
		assert.True(t, reapedC99, "C99 was never enumerated and must be reaped regardless of MaxScopes")
	}

	t.Run("unbounded", func(t *testing.T) { run(t, 0) })
	t.Run("bounded but still covers everything in one pass", func(t *testing.T) { run(t, 100) })
}

// IMPORTANT 3 (fix round 2): the design's own "four kinds of empty" table
// requires an explicit refusal (and a report) when ListScopes returns
// nothing at all — that is treated as "the API is unreliable right now",
// never as "this source genuinely has zero scopes". Before this fix the
// refusal was emergent: reapAbsentScopes's known-resource map happened to
// be empty whenever scopes was empty, so the reap loop happened to have
// nothing to iterate. A refactor deriving scan targets from the source's
// claims instead of this pass's own enumeration would remove that
// accidental safety with no test catching it.
func TestPass_EmptyEnumerationRefusesToReap(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages: []relsync.ScopePage{{Scopes: nil, Complete: true}},
	}
	w := &fakeWriter{}
	r := &fakeReader{owned: []spicedb.Tuple{tup("C9", "bob")}}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})

	require.NoError(t, err)
	assert.Empty(t, w.deletes, "an empty enumeration must never reap, even one that claims to be complete")
	assert.NotEmpty(t, res.ScopeErrors, "an empty enumeration must be reported, not silently treated as a routine no-op")
}

// IMPORTANT 4a (fix round 2): the one gate between an event-driven scoped
// wake and a fleet-wide reap is `!scoped` on the reap condition. Nothing
// in the suite exercised it directly — every other reap test simply never
// sets OnlyScopes, so a dropped `!scoped &&` would still pass every OTHER
// test in this file. This constructs exactly the case that gate exists
// for: a scoped pass with an otherwise-complete, otherwise-reapable
// enumeration.
func TestPass_ScopedPassNeverReapsEvenWithACompleteEnumeration(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": {tup("C1", "alice")}},
	}
	w := &fakeWriter{}
	r := &fakeReader{owned: []spicedb.Tuple{tup("C9", "bob")}}

	_, err := relsync.Pass(ctx, relsync.PassInput{
		Kind: k, Writer: w, Reader: r, OnlyScopes: []relsync.ScopeID{"C1"},
	})

	require.NoError(t, err)
	assert.Empty(t, w.deletes, "a scoped pass must never reap, regardless of how complete the enumeration looks")
}

// IMPORTANT 4b (fix round 2): a scoped wake naming a scope the credential
// cannot list (or that no longer exists) must not silently do nothing —
// the entire point of a scoped pass is that one channel's event produces
// one channel's sync, and a miss here means the event that triggered it
// was dropped on the floor with no trace.
func TestPass_OnlyScopesEntryAbsentFromEnumerationIsReported(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": {tup("C1", "alice")}},
	}
	w := &fakeWriter{}
	r := &fakeReader{}

	res, err := relsync.Pass(ctx, relsync.PassInput{
		Kind: k, Writer: w, Reader: r, OnlyScopes: []relsync.ScopeID{"C1", "C404"},
	})

	require.NoError(t, err)
	assert.Equal(t, 1, res.Processed, "C1 is still processed normally")
	require.Len(t, res.ScopeErrors, 1)
	assert.Equal(t, relsync.ScopeID("C404"), res.ScopeErrors[0].Scope)
}

// IMPORTANT 5a (fix round 2): IgnoreHashes is the only recovery from a
// drifted hash — and half the mitigation for the CRITICAL above, since a
// periodic verify pass is what would have caught an accumulated stale
// sentinel even before it was fixed properly. It had no test at all.
func TestPass_IgnoreHashesForcesAFullDiffEvenWhenTheSentinelMatches(t *testing.T) {
	ctx := context.Background()
	members := []spicedb.Tuple{tup("C1", "alice")}
	k := &fakePassKind{
		pages:   []relsync.ScopePage{{Scopes: []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}}, Complete: true}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": members},
	}
	r := &fakeReader{hashes: map[string]string{"slack_channel:C1": relsync.HashTuples(members)}}
	w := &fakeWriter{}

	_, err := relsync.Pass(ctx, relsync.PassInput{
		Kind: k, Writer: w, Reader: r, IgnoreHashes: true,
	})

	require.NoError(t, err)
	assert.NotEmpty(t, r.scopeReads, "IgnoreHashes must force the full read even though the sentinel matches")
	assert.NotEmpty(t, w.writes, "the verify pass still (re)writes the sentinel, proving the read-back actually happened rather than being skipped anyway")
}

// IMPORTANT 5b (fix round 2): fakePassKind's joinMisses field was declared
// and read (FetchScope's return) but never SET by any test — Ruling 2's
// entire reason for existing (a dropped, unresolvable member is reported,
// never silently absorbed into a shorter list) was dead fixture code.
func TestPass_JoinMissesSumAcrossScopes(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes: []relsync.Scope{
				{ID: "C1", ResourceType: "slack_channel"},
				{ID: "C2", ResourceType: "slack_channel"},
			},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{
			"C1": {tup("C1", "alice")},
			"C2": {tup("C2", "bob")},
		},
		joinMisses: map[relsync.ScopeID]int{"C1": 3, "C2": 2},
	}
	w := &fakeWriter{}
	r := &fakeReader{}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})

	require.NoError(t, err)
	assert.Equal(t, 5, res.JoinMisses, "JoinMisses must sum across every scope this pass fetched")
}

// -----------------------------------------------------------------------
// Enumeration must terminate even when the upstream will not advance
// -----------------------------------------------------------------------

// stuckListKind is the shape that made enumeration unbounded: an upstream
// (or a proxy in front of it) that DROPS the cursor parameter and answers
// every request with the same first page. Its Next advances numerically
// (1 -> 101 -> 201), so nothing about the cursor looks wrong; only the
// CONTENT repeats. With a full page never reporting Complete — which is
// correct, and is what stops a whole-directory reap after page one — nothing
// else bounded the loop.
type stuckListKind struct {
	mu sync.Mutex

	scopes    []relsync.Scope
	listCalls int
	// nextToken advances on every call, so the cursor itself is never the
	// tell. A guard that only compared cursors would never fire here.
	nextToken int
}

func (k *stuckListKind) Name() string { return "stucklist" }

func (k *stuckListKind) Source() relsource.Source { return relsource.Source{Name: "stucklistsync"} }

func (k *stuckListKind) ListScopes(_ context.Context, _ relsync.SourceParams, _ relsync.Cursor) (relsync.ScopePage, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.listCalls++
	k.nextToken += len(k.scopes)
	return relsync.ScopePage{
		Scopes:   k.scopes,
		Next:     relsync.Cursor{Kind: k.Name(), Token: strconv.Itoa(k.nextToken)},
		Complete: false,
	}, nil
}

func (k *stuckListKind) FetchScope(_ context.Context, _ relsync.SourceParams, s relsync.Scope) (relsync.ScopeContent, error) {
	return relsync.ScopeContent{Tuples: []spicedb.Tuple{tup(string(s.ID), "alice")}}, nil
}

func (k *stuckListKind) calls() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.listCalls
}

// Probed before this guard: 5001 requests and 500,100 scopes accumulated into
// the in-memory slice in 0.65s, still not complete. The bound has to be
// STRUCTURAL — a page that repeats what the last one already gave cannot make
// progress, whatever its cursor says — because a page cap is a silent
// truncation, and a truncated enumeration that still reported Complete would
// re-arm the reaper, which is the failure this whole area exists to prevent.
func TestPass_NonAdvancingEnumerationFailsInsteadOfLooping(t *testing.T) {
	ctx := context.Background()
	k := &stuckListKind{scopes: []relsync.Scope{
		{ID: "C1", ResourceType: "slack_channel"},
		{ID: "C2", ResourceType: "slack_channel"},
	}}
	w := &fakeWriter{}
	r := &fakeReader{owned: []spicedb.Tuple{tup("C9", "bob")}}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})

	require.NoError(t, err, "a stuck enumeration is a reported ScopeError, not a fatal Pass error")
	assert.Less(t, k.calls(), 10,
		"the guard must fire within a handful of requests; a page cap would be a silent truncation instead")

	var found bool
	for _, se := range res.ScopeErrors {
		if errors.Is(se.Err, relsync.ErrEnumerationNotAdvancing) {
			found = true
			assert.Contains(t, se.Err.Error(), "stucklist", "the error must name the kind whose upstream is stuck")
		}
	}
	assert.True(t, found, "the stall must be reported as ErrEnumerationNotAdvancing: %v", res.ScopeErrors)

	assert.False(t, res.EnumComplete,
		"a stalled enumeration is not a complete one — reaping on it would sweep every scope it never reached")
	assert.Empty(t, w.deletes, "and so it must reap nothing")
}

// The guard must not fire on a NORMAL enumeration, including one whose pages
// overlap. Pages that repeat some scopes while still contributing new ones are
// advancing, and each distinct scope is enumerated exactly once — a duplicate
// must not be fetched twice, or a pass does double the upstream work it needs.
func TestPass_OverlappingButAdvancingPagesEnumerateEachScopeOnce(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages: []relsync.ScopePage{
			{
				Scopes: []relsync.Scope{
					{ID: "C1", ResourceType: "slack_channel"},
					{ID: "C2", ResourceType: "slack_channel"},
				},
				Next: relsync.Cursor{Kind: "fakepass", Token: "1"},
			},
			{
				// C2 repeats — overlap, not a stall, because C3 is new.
				Scopes: []relsync.Scope{
					{ID: "C2", ResourceType: "slack_channel"},
					{ID: "C3", ResourceType: "slack_channel"},
				},
				Complete: true,
			},
		},
		members: map[relsync.ScopeID][]spicedb.Tuple{
			"C1": {tup("C1", "alice")},
			"C2": {tup("C2", "bob")},
			"C3": {tup("C3", "carol")},
		},
	}
	w := &fakeWriter{}
	r := &fakeReader{}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})

	require.NoError(t, err)
	assert.Empty(t, res.ScopeErrors, "an overlapping but advancing enumeration is not a stall: %v", res.ScopeErrors)
	assert.True(t, res.EnumComplete)
	assert.Equal(t, []relsync.ScopeID{"C1", "C2", "C3"}, k.fetchCalls,
		"each distinct scope is fetched exactly once, however many pages mentioned it")
}

// echoCursorKind returns empty pages forever, handing back the same non-zero
// cursor every time.
type echoCursorKind struct {
	mu        sync.Mutex
	listCalls int
}

func (k *echoCursorKind) Name() string { return "echocursor" }

func (k *echoCursorKind) Source() relsource.Source { return relsource.Source{Name: "echocursorsync"} }

func (k *echoCursorKind) ListScopes(_ context.Context, _ relsync.SourceParams, _ relsync.Cursor) (relsync.ScopePage, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.listCalls++
	return relsync.ScopePage{Next: relsync.Cursor{Kind: k.Name(), Token: "stuck"}}, nil
}

func (k *echoCursorKind) FetchScope(_ context.Context, _ relsync.SourceParams, _ relsync.Scope) (relsync.ScopeContent, error) {
	return relsync.ScopeContent{}, nil
}

func (k *echoCursorKind) calls() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.listCalls
}

// A kind handing back the very cursor it was just given cannot make progress
// by construction, and it is the one stuck shape an EMPTY page can take. Empty
// pages mid-enumeration are legal (Slack's conversations.list filters
// server-side and returns them), so zero-new-scopes alone is deliberately not
// treated as a stall — this is what keeps that exemption from being an
// unbounded loop of its own.
func TestPass_ARepeatedCursorIsAStallEvenWithEmptyPages(t *testing.T) {
	ctx := context.Background()
	k := &echoCursorKind{}
	w := &fakeWriter{}
	r := &fakeReader{}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})

	require.NoError(t, err)
	assert.Less(t, k.calls(), 10, "a cursor that never changes must be caught immediately")

	var found bool
	for _, se := range res.ScopeErrors {
		if errors.Is(se.Err, relsync.ErrEnumerationNotAdvancing) {
			found = true
		}
	}
	assert.True(t, found, "a repeated cursor must be reported as a stall: %v", res.ScopeErrors)
	assert.False(t, res.EnumComplete)
}

// An empty page mid-enumeration is legal and must keep paging: Slack's
// conversations.list applies ExcludeArchived server-side, so a page can come
// back with zero channels and a live next_cursor. A guard that read "zero new
// scopes" as a stall regardless would break that, which is why the stall test
// above needs a page that is non-empty AND repeated.
func TestPass_AnEmptyPageMidEnumerationKeepsPaging(t *testing.T) {
	ctx := context.Background()
	k := &fakePassKind{
		pages: []relsync.ScopePage{
			{Scopes: nil, Next: relsync.Cursor{Kind: "fakepass", Token: "1"}},
			{
				Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}},
				Complete: true,
			},
		},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": {tup("C1", "alice")}},
	}
	w := &fakeWriter{}
	r := &fakeReader{}

	res, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})

	require.NoError(t, err)
	assert.Empty(t, res.ScopeErrors, "a filtered-empty page is not a stall: %v", res.ScopeErrors)
	assert.True(t, res.EnumComplete)
	assert.Equal(t, []relsync.ScopeID{"C1"}, k.fetchCalls)
}
