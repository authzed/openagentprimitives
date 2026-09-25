package relsync_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// labelScope is the scope every test here names — the same slack_channel
// shape sync_test.go's fixtures use.
func labelScope(id string) relsync.Scope {
	return relsync.Scope{ID: relsync.ScopeID(id), ResourceType: "slack_channel"}
}

func encodedName(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// The tuple's exact shape, asserted field by field: it is read back by a
// declaration in another package entirely (spicedb.ScopeLabelBridge), and the
// two agreeing is what makes a name appear on a page.
func TestScopeLabelTuple_BuildsTheTupleTheConsoleReadsBack(t *testing.T) {
	got, ok := relsync.ScopeLabelTuple(context.Background(), labelScope("C0123"), "demo-channel")

	require.True(t, ok)
	assert.Equal(t, spicedb.Tuple{
		ResourceType: "slack_channel",
		ResourceID:   "C0123",
		Relation:     "label",
		SubjectType:  "string",
		SubjectID:    encodedName("demo-channel"),
	}, got)
	assert.Empty(t, got.SubjectRelation, "a bare string subject; a userset here would be ignored by the reader")
}

// A name with nothing renderable left is NOT written. The alternative — an id
// that decodes to an empty title — is a row rendered blank, which is worse
// than the raw id it would have replaced.
func TestScopeLabelTuple_RefusesANameWithNothingRenderableLeft(t *testing.T) {
	for _, name := range []string{"", "   ", "\x00\x01"} {
		got, ok := relsync.ScopeLabelTuple(context.Background(), labelScope("C0123"), name)
		assert.False(t, ok, "%q has no renderable content", name)
		assert.Equal(t, spicedb.Tuple{}, got, "the zero Tuple must never be appended as content")
	}
}

// A hostile name is sanitized BEFORE it reaches SpiceDB, not only on the way
// out: the stored value is what a later reader, a log line and an operator's
// `zed relationship read` all see.
func TestScopeLabelTuple_SanitizesBeforeTheNameEverReachesSpiceDB(t *testing.T) {
	got, ok := relsync.ScopeLabelTuple(context.Background(), labelScope("C0123"), "demo-‮gnp.exe")

	require.True(t, ok)
	assert.Equal(t, encodedName("demo-gnp.exe"), got.SubjectID,
		"the bidi override must not be what lands in the store")
}

// A very long name still produces a legal object id. This is the case that
// would otherwise fail the WRITE — for the whole scope, membership included —
// rather than merely failing to name a row.
func TestScopeLabelTuple_ALongNameStillProducesAWritableID(t *testing.T) {
	got, ok := relsync.ScopeLabelTuple(context.Background(), labelScope("C0123"), strings.Repeat("é", 4000))

	require.True(t, ok)
	assert.LessOrEqual(t, len(got.SubjectID), 1024, "SpiceDB refuses an object id longer than this")
}

// ===========================================================================
// The backfill.
// ===========================================================================

// labelTuple is the content tuple a kind now returns alongside its
// memberships.
func labelTuple(scopeID, name string) spicedb.Tuple {
	t, _ := relsync.ScopeLabelTuple(context.Background(), labelScope(scopeID), name)
	return t
}

// THE backfill property, and the reason a label is ordinary scope content
// rather than a second sentinel.
//
// Pass skips a scope whose stored #relhash still matches what the fetch
// produced — that short-circuit is the entire point of the sentinel. So a
// directory synced BEFORE labels existed has a stored hash covering its
// memberships alone, and if a label were written outside the hashed set, every
// scope would be skipped on every pass forever and the console would keep
// showing raw ids with nothing in the system able to notice.
//
// The fixture is that exact state: the stored sentinel is the hash of the
// PRE-LABEL tuple set, and the kind now returns that same set plus a label.
// Asserted on the WRITE CALL, not on PassResult: a result-only assertion would
// pass for a write that touched everything except the label.
func TestPass_AScopeSyncedBeforeLabelsExistedGainsOneOnTheNextPass(t *testing.T) {
	ctx := context.Background()
	preLabel := []spicedb.Tuple{tup("C1", "alice")}
	withLabel := append(append([]spicedb.Tuple{}, preLabel...), labelTuple("C1", "demo-channel"))

	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": withLabel},
		claims:  append(append([]string{}, defaultFakeClaims...), "slack_channel#label"),
	}
	w := &fakeWriter{}
	r := &fakeReader{
		owned: preLabel,
		// The sentinel an older build left behind: the hash of the membership
		// set alone.
		hashes: map[string]string{"slack_channel:C1": relsync.HashTuples(preLabel)},
	}

	_, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})
	require.NoError(t, err)

	assert.Contains(t, r.scopeReads, "slack_channel:C1",
		"the stored hash no longer matches, so the scope must be READ rather than skipped")
	write := findWriteForResource(t, w.writes, "slack_channel", "C1")
	assert.True(t, writeTouches(write, "slack_channel", "C1", "label"),
		"the backfill is the point: an already-synced scope must pick up its name with no migration, no flag and no forced rewrite")
}

// The other half of the same property, and the one that proves the backfill is
// not simply "write everything every time": once the label is stored, the hash
// matches again and the scope goes back to being skipped. A label that cost a
// full read and write on every pass, forever, would be a regression dressed as
// a feature.
func TestPass_AnAlreadyLabelledScopeGoesBackToBeingSkipped(t *testing.T) {
	ctx := context.Background()
	withLabel := []spicedb.Tuple{tup("C1", "alice"), labelTuple("C1", "demo-channel")}

	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": withLabel},
		claims:  append(append([]string{}, defaultFakeClaims...), "slack_channel#label"),
	}
	w := &fakeWriter{}
	r := &fakeReader{
		owned:  withLabel,
		hashes: map[string]string{"slack_channel:C1": relsync.HashTuples(withLabel)},
	}

	_, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})
	require.NoError(t, err)

	assert.NotContains(t, r.scopeReads, "slack_channel:C1", "an unchanged scope must not be read")
	assert.Empty(t, w.writes, "nor written")
}

// A RENAME is a changed subject id, so the old label tuple has to be DELETED
// in the same write that adds the new one — otherwise the scope accumulates
// one label per name it has ever had, and which one the console shows becomes
// a matter of SpiceDB's stream order.
//
// This is the same hazard diffScope handles by hand for the sentinel. Being
// ordinary content is what gets it handled here for free, and this test is
// what says so rather than assuming it.
func TestPass_ARenamedScopeDeletesItsOldLabelInTheSameWrite(t *testing.T) {
	ctx := context.Background()
	old := []spicedb.Tuple{tup("C1", "alice"), labelTuple("C1", "old-name")}
	renamed := []spicedb.Tuple{tup("C1", "alice"), labelTuple("C1", "new-name")}

	k := &fakePassKind{
		pages: []relsync.ScopePage{{
			Scopes:   []relsync.Scope{{ID: "C1", ResourceType: "slack_channel"}},
			Complete: true,
		}},
		members: map[relsync.ScopeID][]spicedb.Tuple{"C1": renamed},
		claims:  append(append([]string{}, defaultFakeClaims...), "slack_channel#label"),
	}
	w := &fakeWriter{}
	r := &fakeReader{
		owned:  old,
		hashes: map[string]string{"slack_channel:C1": relsync.HashTuples(old)},
	}

	_, err := relsync.Pass(ctx, relsync.PassInput{Kind: k, Writer: w, Reader: r})
	require.NoError(t, err)

	write := findWriteForResource(t, w.writes, "slack_channel", "C1")
	assert.True(t, writeTouches(write, "slack_channel", "C1", "label"), "the new name is written")

	var deletedOld bool
	for _, u := range write.GetUpdates() {
		if u.GetOperation() != v1.RelationshipUpdate_OPERATION_DELETE {
			continue
		}
		rel := u.GetRelationship()
		if rel.GetRelation() == "label" && rel.GetSubject().GetObject().GetObjectId() == encodedName("old-name") {
			deletedOld = true
		}
	}
	assert.True(t, deletedOld,
		"a name rides as a subject id, so the old label is a different tuple and must be removed, not left to accumulate")
}
