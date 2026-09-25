package onepassword

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// namedGroupServer serves GET /scim/Groups/{id} as a conformant singular
// resource — `displayName` and `members`, no ListResponse envelope — with the
// name under the caller's control. That attribute is what this kind read on
// every group fetch and discarded.
func namedGroupServer(t *testing.T, displayName string, memberIDs []string) *httptest.Server {
	t.Helper()
	members := make([]map[string]string, 0, len(memberIDs))
	for _, id := range memberIDs {
		members = append(members, map[string]string{"value": id})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "g1", "displayName": displayName, "members": members,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func labelTupleIn(t *testing.T, tuples []spicedb.Tuple) (spicedb.Tuple, bool) {
	t.Helper()
	for _, tup := range tuples {
		if tup.Relation == relsource.LabelRelation {
			return tup, true
		}
	}
	return spicedb.Tuple{}, false
}

// SCIM keys a Group by an opaque id and carries the human name in a separate
// attribute. Storing that attribute is what turns a console row from a bare
// UUID into `demo-engineers`.
//
// The group is deliberately EMPTY, and that is the case worth pinning: an
// empty membership returns from fetchAllGroupMembers before any paging
// arithmetic runs, so a displayName captured anywhere later would be lost for
// exactly the groups an operator is most likely to be investigating.
func TestKind_FetchScopeStoresTheGroupDisplayNameEvenForAnEmptyGroup(t *testing.T) {
	srv := namedGroupServer(t, "demo-engineers", nil)

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))
	require.NoError(t, err)

	assert.Equal(t, []spicedb.Tuple{{
		ResourceType: onepasswordGroupResourceType,
		ResourceID:   "g1",
		Relation:     relsource.LabelRelation,
		SubjectType:  relsource.LabelSubjectType,
		SubjectID:    base64.RawURLEncoding.EncodeToString([]byte("demo-engineers")),
	}}, content.Tuples, "an empty group's only content is its own name — and it must still get one")
}

// A bridge that omits displayName leaves the row rendering its raw id, the
// pre-label behaviour, rather than storing an id that decodes to nothing.
func TestKind_FetchScopeWritesNoLabelWhenTheBridgeReportsNoDisplayName(t *testing.T) {
	srv := namedGroupServer(t, "", nil)

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))
	require.NoError(t, err)

	_, found := labelTupleIn(t, content.Tuples)
	assert.False(t, found, "an absent displayName must produce no label at all")
}

// The name must not cost a request of its own. It rides on the same GET
// /scim/Groups/{id} the membership read already makes, and a second call per
// group would multiply this kind's request count against a rate-limited
// customer-hosted bridge for a cosmetic field.
func TestKind_NamingAGroupCostsNoExtraRequest(t *testing.T) {
	var groupGets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == scimGroupsPath+"/g1" {
			groupGets++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "g1", "displayName": "demo-engineers", "members": []any{}})
	}))
	t.Cleanup(srv.Close)

	_, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))
	require.NoError(t, err)

	assert.Equal(t, 1, groupGets, "the displayName rides on the membership read this kind already makes")
}

// TestKind_ScopeLabelBridgeMatchesTheTupleTheSyncWrites closes the loop
// between the two halves of this feature: FetchScope mints a tuple,
// ScopeLabelBridges declares where to read one, and a mismatch is SILENT —
// the read runs, resolves nothing, and every row shows a raw id, which looks
// exactly like a kind that stores no names at all.
func TestKind_ScopeLabelBridgeMatchesTheTupleTheSyncWrites(t *testing.T) {
	srv := namedGroupServer(t, "demo-engineers", nil)

	content, err := newKind().FetchScope(ctx, credsFor(t, srv), scope("g1"))
	require.NoError(t, err)
	written, found := labelTupleIn(t, content.Tuples)
	require.True(t, found, "precondition: the sync must have written a label")

	bridges := newKind().ScopeLabelBridges()
	require.Len(t, bridges, 1, "onepassword_group is the only definition this kind enumerates as a scope")
	b := bridges[0]

	assert.Equal(t, written.ResourceType, b.ScopeDefinition)
	assert.Equal(t, written.SubjectType, b.BridgeDefinition)
	assert.Equal(t, written.Relation, b.BridgeRelation)
	assert.Equal(t, spicedb.ShapeNameOnSubject, b.Shape,
		"scope on the resource, name on the subject — the bridged shape would read the wrong definition entirely")

	title, href := b.Decoder.Decode(written.SubjectID)
	assert.Equal(t, "demo-engineers", title, "the declared decoder must read back exactly what the sync wrote")
	assert.Empty(t, href, "a free-form displayName is text, never a link target")
	assert.Equal(t, resourcedisplay.DecoderB64Text, b.Decoder)
}

// The relation the sync writes has to be CLAIMED: the sync's own diff-and-prune
// deletes it, and diff-and-prune is only correct when nothing else writes what
// it deletes. A label has one extra edge on top of that — it is what the
// console SHOWS a human.
func TestKind_ClaimsTheLabelRelationItWrites(t *testing.T) {
	got, ok := relsync.Get(KindName)
	require.True(t, ok)

	src := got.Source()
	assert.Contains(t, src.Claims, "onepassword_group#label")
	assert.True(t, relsource.Owns(src, onepasswordGroupResourceType, relsource.LabelRelation),
		"ownership, not merely permission: CheckWrite's silence on an unclaimed relation is not ownership")
	assert.NotContains(t, src.SubjectIdentityClaims, "onepassword_group#label",
		"a label's subject is an encoded name, never a user; probing it would return nothing forever")
}
