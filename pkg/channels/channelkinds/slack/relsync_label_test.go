package slack

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// labelMux serves the four calls FetchScope makes, with the channel's name
// under the caller's control — the field this kind used to read and discard.
func labelMux(channelName string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", jsonHandler(map[string]any{
		"ok":      true,
		"channel": map[string]any{"id": "C1", "name": channelName, "is_private": false},
	}))
	mux.HandleFunc("/conversations.members", jsonHandler(map[string]any{
		"ok": true, "members": []string{}, "response_metadata": map[string]any{"next_cursor": ""},
	}))
	mux.HandleFunc("/auth.test", jsonHandler(map[string]any{"ok": true, "team_id": "T1"}))
	return mux
}

func fetchC1(t *testing.T) relsync.ScopeContent {
	t.Helper()
	content, err := (&SyncKind{}).FetchScope(context.Background(), testCreds(),
		relsync.Scope{ID: "C1", ResourceType: slackChannelResourceType})
	require.NoError(t, err)
	return content
}

// The name arrives on a conversations.info call this kind has always made and
// has always thrown away. Storing it is what turns a console row from
// `slack_channel:C08TUFTDYTC` into `demo-channel`.
//
// The membership list is deliberately EMPTY: a channel with no resolvable
// members still gets named, and a label that only appeared alongside members
// would leave exactly the rows an operator is most likely to be investigating
// unnamed.
func TestSlackKind_FetchScopeStoresTheChannelName(t *testing.T) {
	startDirectoryTestServer(t, labelMux("demo-channel"))

	content := fetchC1(t)

	assert.Contains(t, content.Tuples, spicedb.Tuple{
		ResourceType: slackChannelResourceType,
		ResourceID:   "C1",
		Relation:     relsource.LabelRelation,
		SubjectType:  relsource.LabelSubjectType,
		SubjectID:    base64.RawURLEncoding.EncodeToString([]byte("demo-channel")),
	}, "the channel's name must be stored as scope content, which is what makes it hashed, diffed and backfilled")
}

// A workspace whose channel name Slack does not report leaves the row
// rendering its raw id — the pre-label behaviour — rather than storing an id
// that decodes to nothing.
func TestSlackKind_FetchScopeWritesNoLabelWhenSlackReportsNoName(t *testing.T) {
	startDirectoryTestServer(t, labelMux(""))

	for _, tup := range fetchC1(t).Tuples {
		assert.NotEqual(t, relsource.LabelRelation, tup.Relation,
			"an empty name must produce no label at all, not one that decodes to nothing")
	}
}

// TestSlackKind_ScopeLabelBridgeMatchesTheTupleTheSyncWrites closes the loop
// between the two halves of this feature, which live in different files and in
// different packages: FetchScope mints a tuple, ScopeLabelBridges declares
// where to read one, and a mismatch between them is SILENT — the read runs,
// resolves nothing, and every row shows a raw id, which is indistinguishable
// from a kind that stores no names at all.
//
// Modelled on the GitHub kind's test of the same name, and it has to assert
// the SHAPE too: this bridge is the inverted one, and reading it as GitHub's
// would stream the `string` definition and match nothing.
func TestSlackKind_ScopeLabelBridgeMatchesTheTupleTheSyncWrites(t *testing.T) {
	startDirectoryTestServer(t, labelMux("demo-channel"))

	var written spicedb.Tuple
	for _, tup := range fetchC1(t).Tuples {
		if tup.Relation == relsource.LabelRelation {
			written = tup
		}
	}
	require.NotEmpty(t, written.ResourceID, "precondition: the sync must have written a label")

	bridges := (&SyncKind{}).ScopeLabelBridges()
	require.Len(t, bridges, 1, "slack_channel is the only definition this kind enumerates as a scope")
	b := bridges[0]

	assert.Equal(t, written.ResourceType, b.ScopeDefinition, "the bridge must name the definition the tuple's RESOURCE is on")
	assert.Equal(t, written.SubjectType, b.BridgeDefinition, "and the definition the name rides on as a SUBJECT")
	assert.Equal(t, written.Relation, b.BridgeRelation)
	assert.Equal(t, spicedb.ShapeNameOnSubject, b.Shape,
		"scope on the resource, name on the subject — the bridged shape would read the wrong definition entirely")

	title, href := b.Decoder.Decode(written.SubjectID)
	assert.Equal(t, "demo-channel", title, "the declared decoder must read back exactly what the sync wrote")
	assert.Empty(t, href, "a channel name is text, never a link target")
	assert.Equal(t, resourcedisplay.DecoderB64Text, b.Decoder)
}

// The relation the sync writes has to be CLAIMED, or anything else in the tree
// may write it — and a label is what the console SHOWS a human, so an
// unclaimed one lets some other writer decide what a synced channel is called
// on the page an operator uses to judge the sync.
func TestSlackKind_ClaimsTheLabelRelationItWrites(t *testing.T) {
	got, ok := relsync.Get(KindName)
	require.True(t, ok)

	src := got.Source()
	assert.Contains(t, src.Claims, "slack_channel#label", "this kind writes the label; it must claim it")
	assert.True(t, relsource.Owns(src, slackChannelResourceType, relsource.LabelRelation),
		"ownership, not merely permission: the sync's own diff deletes this relation, and CheckWrite's silence on an unclaimed relation is not ownership")
	assert.NotContains(t, src.SubjectIdentityClaims, "slack_channel#label",
		"a label's subject is an encoded name, never a user; probing it would return nothing forever")
}

// The relation must be DECLARED, or SpiceDB rejects the whole WriteSchema and
// the sync writes nothing at all. Asserted against the fragment this kind
// actually contributes rather than against a copy of the text.
func TestSlackKind_FragmentDeclaresTheLabelRelation(t *testing.T) {
	frag := (&Kind{}).SpiceDBSchemaFragment()
	require.NotNil(t, frag)

	assert.Contains(t, frag.RawZed, "relation label: string",
		"an undeclared relation makes the whole schema write fail, not merely the label")
	assert.NotContains(t, frag.RawZed, "definition string",
		"the label's subject type stays base-scaffold-owned, exactly as the sentinel's does")
}
