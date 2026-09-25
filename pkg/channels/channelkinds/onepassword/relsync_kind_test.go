package onepassword

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// ctx is shared across this file's tests — none of them need cancellation
// or a per-test deadline, and the brief's own test bodies pass it bare.
var ctx = context.Background()

// newKind constructs a fresh *SyncKind. The per-install endpoint no longer
// lives on this type (task 4b) — it rides on relsync.SourceParams.Endpoint,
// which credsFor below sets from the same httptest server every call site
// already points at.
func newKind() *SyncKind {
	return &SyncKind{}
}

// credsFor is a fixture credential pointed at srv's own URL via Endpoint —
// standing in for the per-install RelationshipSourceSpec.BaseURL a real
// deployment resolves onto Credentials before calling this kind (see
// SyncKind's own doc). Pass a nil srv when no HTTP round-trip should ever
// happen (TestKind_RefusesAForeignCursor builds its own Credentials
// directly instead, since it wants a specific unreachable endpoint). The
// fake servers in this file never inspect the token value, only that
// Authorization is set (doGet always sets it).
func credsFor(t *testing.T, srv *httptest.Server) relsync.SourceParams {
	t.Helper()
	var endpoint string
	if srv != nil {
		endpoint = srv.URL
	}
	return relsync.SourceParams{Token: sensitive.NewSensitiveValue([]byte("scim-test-token")), Endpoint: endpoint}
}

// scope builds the relsync.Scope this kind's FetchScope expects for group id.
func scope(id string) relsync.Scope {
	return relsync.Scope{ID: relsync.ScopeID(id), ResourceType: onepasswordGroupResourceType}
}

// canonicalOf computes the canonical subject id a verified email produces.
// Canonical()'s encoding depends only on the (lower-cased) email when one is
// present — EmailReference and FromExternal produce a byte-identical id for
// the same address, differing only in the emailVerified bit the id itself
// never carries (see identity.Principal.Canonical's doc) — matching how the
// Slack kind's own tests compute their expected id.
func canonicalOf(t *testing.T, email string) string {
	t.Helper()
	c, err := identity.EmailReference(identity.Email(email)).Canonical()
	require.NoError(t, err, "precondition: a non-empty email must canonicalize")
	return c.String()
}

// writeGroupResource writes the REAL GET /scim/Groups/{id} response: a SCIM
// core Group resource (RFC 7643 §4.2) carrying `members`, and no envelope.
// totalResults/itemsPerPage/startIndex are ListResponse fields (RFC 7644
// §3.4.2) — a singular resource GET does not return a ListResponse, so it does
// not carry them.
//
// This replaces a fixture that wrapped the group in a ListResponse envelope no
// conformant bridge emits. Shaping a fixture to the code rather than to the
// upstream is what let a 250-member group sync as 100 members with every test
// green: the code read totalResults, the fixture obligingly supplied it, and
// nothing in the suite ever saw the shape a real bridge sends.
func writeGroupResource(t *testing.T, w http.ResponseWriter, ids []string) {
	t.Helper()
	members := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		members = append(members, map[string]string{"value": id})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "g1", "members": members})
}

// writeEnvelopedMembersPage writes the OTHER shape: one page of a single
// group's members wrapped in the List-response triple
// (totalResults/itemsPerPage/startIndex), scoped to this one group's `members`
// rather than to multiple Group resources. It is kept because a bridge that
// pages an oversized group's membership has to say so somehow, and this is the
// only form fetchAllGroupMembers can page against. more=true makes
// totalResults exceed what this page reports, so a second request is required
// to reach completion.
func writeEnvelopedMembersPage(t *testing.T, w http.ResponseWriter, ids []string, more bool) {
	t.Helper()
	members := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		members = append(members, map[string]string{"value": id})
	}
	total := len(ids)
	if more {
		total = len(ids) + 1 // any excess signals "more remain" to the client
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":           "g1",
		"members":      members,
		"totalResults": total,
		"itemsPerPage": len(ids),
		"startIndex":   1,
	})
}

// memberServer builds an httptest server backing a single group "g1" whose
// membership is emails' keys. Each key's SCIM User record carries the map's
// value as its primary email — EXCEPT the literal "nobody@example.com",
// which this fixture reserves to model a member whose upstream User
// resource carries no email attribute at all (an unconfirmed or external
// account 1Password's directory never got an address for): the join-miss
// case. That string is never actually served as an email value; it is this
// fixture's own placeholder for "resolves to nobody," matching the
// TestKind_UnresolvableMemberIsDroppedAndCounted body's own comment ("alice
// resolves to a platform user; nobody@ does not").
func memberServer(t *testing.T, emails map[string]string) *httptest.Server {
	t.Helper()
	ids := make([]string, 0, len(emails))
	for id := range emails {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic tuple order for assertions

	mux := http.NewServeMux()
	mux.HandleFunc("/scim/Groups/g1", func(w http.ResponseWriter, r *http.Request) {
		writeGroupResource(t, w, ids)
	})
	mux.HandleFunc("/scim/Users/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/scim/Users/")
		email := emails[id]
		var scimEmails []map[string]any
		if email != "" && email != "nobody@example.com" {
			scimEmails = []map[string]any{{"value": email, "primary": true}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "emails": scimEmails})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A truncated enumeration is non-empty, well-formed and short. The pass
// gates deletion on Complete, so reporting true here arms the reaper
// against every group we never reached.
func TestKind_TruncatedListReportsIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"totalResults": 5,
			"itemsPerPage": 2,
			"startIndex":   1,
			"Resources": []map[string]any{
				{"id": "g1", "displayName": "Engineers"},
				{"id": "g2", "displayName": "Founders"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	page, err := newKind().ListScopes(ctx, credsFor(t, srv), relsync.Cursor{})
	require.NoError(t, err)

	assert.False(t, page.Complete, "totalResults=5 but only 2 returned — must never claim Complete=true")
	require.Len(t, page.Scopes, 2, "the page must still be well-formed, not discarded for being partial")
	assert.Equal(t, relsync.ScopeID("g1"), page.Scopes[0].ID)
	assert.Equal(t, onepasswordGroupResourceType, page.Scopes[0].ResourceType)
	assert.Equal(t, relsync.Cursor{Kind: KindName, Token: "3"}, page.Next,
		"the next startIndex is 1 (this page's start) + 2 (resources returned)")
}

// RFC 7644 does not require totalResults, and a bridge that omits it (or
// reports it per-page) is conformant. It decodes to 0, so a predicate of the
// form `nextStart > totalResults` reads 101 > 0 and reports Complete=true after
// the very first FULL page — which arms relsync.Pass's whole-type reap against
// every group from 101 onward.
//
// A page that filled the requested count is never the end, whatever
// totalResults says. The cost of being wrong is one extra request returning
// zero resources, which then reports complete; the cost of the old reading is
// deleting every group past page one.
func TestKind_AFullPageWithNoTotalResultsIsNotComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resources := make([]map[string]any, 0, scimPageSize)
		for i := range scimPageSize {
			resources = append(resources, map[string]any{"id": fmt.Sprintf("g%d", i+1)})
		}
		w.Header().Set("Content-Type", "application/json")
		// No totalResults, no itemsPerPage: exactly what a conformant bridge
		// that does not report them sends.
		_ = json.NewEncoder(w).Encode(map[string]any{"Resources": resources})
	}))
	t.Cleanup(srv.Close)

	page, err := newKind().ListScopes(ctx, credsFor(t, srv), relsync.Cursor{})
	require.NoError(t, err)

	require.Len(t, page.Scopes, scimPageSize, "precondition: the fixture must return a full page")
	assert.False(t, page.Complete,
		"a full page with no totalResults must never claim the enumeration finished — that arms the whole-type reap")
	assert.Equal(t, relsync.Cursor{Kind: KindName, Token: "101"}, page.Next,
		"and it must hand back a usable cursor, or the pass cannot page past the first 100 groups")
}

// The other half of Ruling D, which is what keeps the extra request bounded: a
// SHORT page with no totalResults has nothing left to page toward, so it
// completes rather than looping forever on an ever-advancing startIndex.
func TestKind_ShortAndEmptyPagesStillComplete(t *testing.T) {
	cases := []struct {
		name         string
		resourceIDs  []string
		totalResults int
		wantComplete bool
		wantNext     relsync.Cursor
	}{
		{
			name:         "a short page with no totalResults: nothing left to page toward",
			resourceIDs:  []string{"g1", "g2"},
			wantComplete: true,
		},
		{
			name:         "an empty page: a request past the end, or an upstream anomaly",
			wantComplete: true,
		},
		{
			name:         "a short page whose totalResults says more remain: the truncated case still reports incomplete",
			resourceIDs:  []string{"g1", "g2"},
			totalResults: 5,
			wantNext:     relsync.Cursor{Kind: KindName, Token: "3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				resources := make([]map[string]any, 0, len(tc.resourceIDs))
				for _, id := range tc.resourceIDs {
					resources = append(resources, map[string]any{"id": id})
				}
				body := map[string]any{"Resources": resources}
				if tc.totalResults > 0 {
					body["totalResults"] = tc.totalResults
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			t.Cleanup(srv.Close)

			page, err := newKind().ListScopes(ctx, credsFor(t, srv), relsync.Cursor{})
			require.NoError(t, err)

			assert.Equal(t, tc.wantComplete, page.Complete)
			assert.Equal(t, tc.wantNext, page.Next)
		})
	}
}

// FetchScope is atomic: page 2 failing yields an error and no partial set,
// because the per-scope prune deletes whatever is absent. Only reachable in
// the ENVELOPED shape, which is the only form that can say "more remain".
func TestKind_FetchScopeIsAtomicAcrossPages(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			writeEnvelopedMembersPage(t, w, []string{"u1"} /*more=*/, true)
			return
		}
		w.WriteHeader(http.StatusInternalServerError) // page 2 fails
	}))
	t.Cleanup(srv.Close)

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))

	require.Error(t, err, "a scope whose second page failed must error, never return page one")
	assert.Empty(t, content.Tuples, "a partial set would let the per-scope prune delete the members it never reached")
	assert.Zero(t, content.JoinMisses)
	assert.Equal(t, 2, hits, "both pages must have actually been attempted")
}

// The truncation this refuses to perform: totalResults is a ListResponse
// field, and GET /scim/Groups/{id} returns a Group resource, not a
// ListResponse — so it decodes to 0. Reading `next > totalResults` as "done"
// returned after ONE page, and a 250-member group synced as 100 members. Since
// FetchScope is authoritative for the scope, the per-scope prune then deleted
// the other 150 memberships. People lose access, quietly.
//
// There is no correct guess available here: a full member page with no
// totalResults to page against could be a truncated list or a complete one,
// and the two are indistinguishable on the wire. Failing the scope surfaces on
// the CR's Ready condition and in monitoring; truncating surfaces nowhere.
func TestKind_FullMemberPageWithNoTotalResultsFailsTheScope(t *testing.T) {
	ids := make([]string, 0, scimPageSize)
	for i := range scimPageSize {
		ids = append(ids, fmt.Sprintf("u%d", i+1))
	}
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeGroupResource(t, w, ids)
	}))
	t.Cleanup(srv.Close)

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))

	require.Error(t, err, "a full member page with nothing to page against must fail the scope, not truncate it")
	assert.ErrorIs(t, err, errMemberPageUnbounded)
	assert.Equal(t, relsync.ScopeContent{}, content,
		"the atomicity contract: a failed scope reports nothing, or the prune deletes what it never read")
	assert.Equal(t, 1, hits, "it must refuse on the first page rather than guess a second startIndex")
}

// The same truncation, surviving a totalResults that is PRESENT. A bridge
// reporting per-page bookkeeping — `totalResults` equal to `itemsPerPage` —
// honours startIndex but says "100 of 100" on the first page of a 250-member
// group. Read as a grand total, that arithmetic says the read is finished, so
// the group synced as 100 members and the per-scope prune deleted the other
// 150: MAJOR 4 unfixed for this shape.
//
// `totalResults` only ends the read when it PROVES more do not remain. A value
// that merely equals what is already held says nothing about a larger group,
// and MAJOR 3's own reasoning already treats per-page reporting as conformant.
// A group of exactly 100 with an accurate `totalResults: 100` is refused by the
// same rule, and that is not collateral damage — on the wire it is
// indistinguishable from this case.
func TestKind_FullMemberPageWhoseTotalResultsProvesNothingFailsTheScope(t *testing.T) {
	ids := make([]string, 0, scimPageSize)
	for i := range scimPageSize {
		ids = append(ids, fmt.Sprintf("u%d", i+1))
	}
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		members := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			members = append(members, map[string]string{"value": id})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "g1",
			"members": members,
			// Per-page bookkeeping, not a grand total. The group really has 250.
			"totalResults": scimPageSize,
			"itemsPerPage": scimPageSize,
			"startIndex":   1,
		})
	}))
	t.Cleanup(srv.Close)

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))

	require.Error(t, err, "a totalResults that merely equals what is already held proves nothing")
	assert.ErrorIs(t, err, errMemberPageUnbounded)
	assert.Equal(t, relsync.ScopeContent{}, content)
	assert.Equal(t, 1, hits)
}

// The counterpart that must keep working: a totalResults that genuinely
// exceeds what is held DOES prove more remain, so paging continues and the
// whole 250 is read.
func TestKind_FullMemberPageWithAProvingTotalResultsPagesToCompletion(t *testing.T) {
	const total = 250
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
		if start < 1 {
			start = 1
		}
		members := make([]map[string]string, 0, scimPageSize)
		for i := start; i < start+scimPageSize && i <= total; i++ {
			members = append(members, map[string]string{"value": fmt.Sprintf("u%d", i)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "g1", "members": members,
			"totalResults": total, "itemsPerPage": len(members), "startIndex": start,
		})
	}))
	t.Cleanup(srv.Close)

	ids, _, err := newKind().fetchAllGroupMembers(ctx, credsFor(t, srv), "g1")

	require.NoError(t, err, "a totalResults larger than what is held is exactly the proof the refusal asks for")
	assert.Len(t, ids, total, "every member must be read, not just the first page")
}

// A group whose size is an exact MULTIPLE of scimPageSize ends on a full page
// whose truthful totalResults equals what is now held — the same arithmetic a
// per-page-bookkeeping bridge produces on its FIRST page, and the reason the
// refusal cannot be re-decided per page.
//
// By the last page the value has already proven itself: page one reported
// totalResults=200 while holding 100. A refusal here fails the scope on every
// pass, so the group never syncs at all — worse than the truncation the rule
// exists to prevent, since a truncated read at least writes someone.
func TestKind_AGroupAnExactMultipleOfThePageSizeReadsToCompletion(t *testing.T) {
	for _, total := range []int{scimPageSize, 2 * scimPageSize, 3 * scimPageSize} {
		t.Run(fmt.Sprintf("%d members", total), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
				if start < 1 {
					start = 1
				}
				members := make([]map[string]string, 0, scimPageSize)
				for i := start; i < start+scimPageSize && i <= total; i++ {
					members = append(members, map[string]string{"value": fmt.Sprintf("u%d", i)})
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "g1", "members": members,
					"totalResults": total, "itemsPerPage": len(members), "startIndex": start,
				})
			}))
			t.Cleanup(srv.Close)

			ids, _, err := newKind().fetchAllGroupMembers(ctx, credsFor(t, srv), "g1")

			if total == scimPageSize {
				// The one page this bridge ever serves is byte-identical to a
				// per-page-bookkeeping bridge's first page, so it is refused —
				// the documented trade, pinned here so widening the proof does
				// not silently reopen the truncation.
				require.ErrorIs(t, err, errMemberPageUnbounded,
					"a single full page cannot prove itself, whoever served it")
				return
			}
			require.NoError(t, err, "totalResults proved more remained on an earlier page; it cannot stop proving it")
			assert.Len(t, ids, total, "every member must be read, not just the first page")
		})
	}
}

// A SHORT member page with no totalResults is the ordinary singular-GET
// response: the whole membership arrived in one body, there is nothing to page
// toward, and it must sync normally rather than inherit the refusal above.
func TestKind_ShortMemberPageWithNoTotalResultsSyncsWholly(t *testing.T) {
	srv := memberServer(t, map[string]string{"u1": "alice@example.com", "u2": "bob@example.com"})

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))

	require.NoError(t, err, "the common case must not be collateral damage from the unbounded-page refusal")
	assert.Len(t, content.Tuples, 2)
}

// A bridge that ignores startIndex on a singular GET serves page one forever.
// Trusting totalResults alone would loop until the context died; returning the
// accumulated ids would report each member once and look correct while the
// rest were pruned. Named error, neither.
func TestKind_NonAdvancingMemberPaginationFailsRatherThanLoops(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		// The same member every time, while claiming 250 in total: startIndex
		// is being ignored, and the claim never runs out on its own.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "g1",
			"members":      []map[string]string{{"value": "u1"}},
			"totalResults": 250,
			"startIndex":   1,
		})
	}))
	t.Cleanup(srv.Close)

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))

	require.Error(t, err)
	assert.ErrorIs(t, err, errMemberPaginationStalled)
	assert.Equal(t, relsync.ScopeContent{}, content)
	assert.Less(t, hits, 5, "it must refuse as soon as a page contributes nothing new, not spin")
}

// A deleted group is distinct from an empty one: missing maps to
// ErrScopeGone.
func TestKind_MissingGroupMapsToErrScopeGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("ggone"))

	require.Error(t, err)
	assert.ErrorIs(t, err, relsync.ErrScopeGone)
	assert.Equal(t, relsync.ScopeContent{}, content)
}

// A deleted group is distinct from an empty one: an existing, empty group
// returns empty content, no error.
func TestKind_EmptyGroupReturnsEmptyContentNotGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeGroupResource(t, w, nil)
	}))
	t.Cleanup(srv.Close)

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))

	require.NoError(t, err)
	assert.Equal(t, relsync.ScopeContent{}, content,
		"an existing, empty group must be distinct from ErrScopeGone — no error, and no tuples")
}

// The identity join: an upstream email becomes a canonical subject, never a
// raw email as an object id.
func TestKind_JoinsMembersByCanonicalizedEmail(t *testing.T) {
	srv := memberServer(t, map[string]string{"u1": "Alice@Example.com"})

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))
	require.NoError(t, err)

	require.Len(t, content.Tuples, 1)
	assert.Equal(t, spicedb.Tuple{
		ResourceType: onepasswordGroupResourceType, ResourceID: "g1", Relation: "member",
		SubjectType: "user", SubjectID: canonicalOf(t, "alice@example.com"),
	}, content.Tuples[0], "the identity link must carry the CANONICALIZED (lower-cased, base64url-encoded) email as the subject id")
	assert.NotEqual(t, "Alice@Example.com", content.Tuples[0].SubjectID, "the raw email must never appear as a SpiceDB object id")
	assert.NotEqual(t, "alice@example.com", content.Tuples[0].SubjectID, "the lower-cased-but-not-encoded email must never appear as a SpiceDB object id either")
	assert.Equal(t, 0, content.JoinMisses)
}

// A member who resolves to nobody is DROPPED and COUNTED. Writing 1 of 2
// silently is what the no-silent-errors rule exists to prevent.
func TestKind_UnresolvableMemberIsDroppedAndCounted(t *testing.T) {
	// alice resolves to a platform user; nobody@ does not.
	srv := memberServer(t, map[string]string{"u1": "alice@example.com", "u2": "nobody@example.com"})
	t.Cleanup(srv.Close)

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))

	require.NoError(t, err)
	require.Len(t, content.Tuples, 1, "the unresolvable member is DROPPED — it must resolve to nobody")
	assert.Equal(t, canonicalOf(t, "alice@example.com"), content.Tuples[0].SubjectID)
	assert.Equal(t, 1, content.JoinMisses,
		"and COUNTED — writing 1 of 2 silently is exactly what the no-silent-errors rule exists to prevent")
}

// A cursor another kind minted must be refused, not read as an offset.
func TestKind_RefusesAForeignCursor(t *testing.T) {
	foreign := relsync.Cursor{Kind: "slack", Token: "dGVhbTpDMDE5"}

	// No server is reached: ForKind must refuse before any HTTP round-trip,
	// which is exactly why an unreachable endpoint is safe to use here.
	creds := relsync.SourceParams{Token: sensitive.NewSensitiveValue([]byte("scim-test-token")), Endpoint: "http://onepassword-test.invalid"}
	_, err := newKind().ListScopes(ctx, creds, foreign)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "slack")
	assert.Contains(t, err.Error(), KindName)
}

// Every relation the kind emits is claimed by its Source.
func TestKind_WritesOnlyWhatItClaims(t *testing.T) {
	srv := memberServer(t, map[string]string{"u1": "grace@example.com"})

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))
	require.NoError(t, err)
	require.NotEmpty(t, content.Tuples, "precondition: this test must exercise at least one written relation")

	claims := DirectorySyncSource.Claims
	for _, tup := range content.Tuples {
		rel := tup.ResourceType + "#" + tup.Relation
		assert.Contains(t, claims, rel, "every relation the kind writes must be claimed by its Source")
	}

	// The kind must self-register under KindName with that same Source —
	// an unregistered claim owner is inert (see directory_sync_source_test.go's
	// TestSourceIsRegistered for the package-level half of this check).
	got, ok := relsync.Get(KindName)
	require.True(t, ok, "the onepassword kind must self-register via init()")
	assert.Equal(t, DirectorySyncSource, got.Source())
}

// A kind that needs an endpoint and is given none must refuse, not dial
// something wrong or nil-deref. The registered singleton (relsync.Get)
// itself has no endpoint of its own to fall back to — the caller must
// supply one via Credentials.Endpoint — so a zero-value SyncKind and a
// zero-value Credentials is exactly the shape a real, unconfigured
// RelationshipSource would drive this kind with.
func TestSyncKind_RefusesAnEmptyEndpoint(t *testing.T) {
	_, err := (&SyncKind{}).ListScopes(ctx, relsync.SourceParams{}, relsync.Cursor{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint")
}

// FetchScope must refuse the same way ListScopes does — an empty endpoint
// is unusable regardless of which method dereferences it first.
func TestSyncKind_FetchScopeRefusesAnEmptyEndpoint(t *testing.T) {
	_, err := (&SyncKind{}).FetchScope(ctx, relsync.SourceParams{}, scope("g1"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint")
}
