package spicedb

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
)

// Pure-logic tests, exactly like source_scope_labels_test.go's: nothing here
// touches the network, and nothing calls relsource.Register, so no build
// constraint is needed. The live read against a real SpiceDB is covered in
// source_scopes_integration_test.go.

// channelLabelBridge is the shape a sync that STORES its own name declares —
// the Slack and 1Password kinds' ScopeLabelBridges, reconstructed here so this
// package's tests do not import a channel kind.
func channelLabelBridge() ScopeLabelBridge {
	return ScopeLabelBridge{
		ScopeDefinition:  "slack_channel",
		BridgeDefinition: "string",
		BridgeRelation:   "label",
		Shape:            ShapeNameOnSubject,
		Decoder:          resourcedisplay.DecoderB64Text,
	}
}

// labelID encodes a name the way relsync.ScopeLabelTuple does.
func labelID(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// The read has to look at the right definition, and under the two shapes that
// is two DIFFERENT fields of the same struct. Reading the bridged shape's
// BridgeDefinition for a stored-name bridge would stream `string` — a
// definition with no relations at all — and resolve nothing, which on the page
// is indistinguishable from a directory that stores no names.
func TestScopeLabelBridge_ReadsTheDefinitionThatActuallyHoldsTheRelation(t *testing.T) {
	repo := repoBridge()
	assert.Equal(t, "github_repo_url", repo.readDefinition(), "the bridged shape's relation lives on the URL type")
	assert.Equal(t, "github_repo", repo.subjectDefinition(), "and its subject is the scope")

	label := channelLabelBridge()
	assert.Equal(t, "slack_channel", label.readDefinition(), "a stored name's relation lives on the SCOPE type")
	assert.Equal(t, "string", label.subjectDefinition(), "and its subject carries the name")
}

// The side-picking, asserted directly because getting it backwards is silent:
// the read runs, rows come back, and every join misses.
func TestScopeLabelBridge_PicksTheScopeAndNameHalvesPerShape(t *testing.T) {
	// github_repo_url:<b64 url>#repo@github_repo:1005857813
	scopeID, nameID := repoBridge().scopeAndNameIDs("aHR0cHM6Ly9leGFtcGxl", "1005857813")
	assert.Equal(t, "1005857813", scopeID, "under the bridged shape the SUBJECT is the scope")
	assert.Equal(t, "aHR0cHM6Ly9leGFtcGxl", nameID)

	// slack_channel:C0123#label@string:<b64 name>
	scopeID, nameID = channelLabelBridge().scopeAndNameIDs("C0123", labelID("demo-channel"))
	assert.Equal(t, "C0123", scopeID, "under the stored-name shape the RESOURCE is the scope")
	assert.Equal(t, labelID("demo-channel"), nameID)
}

// The zero value must stay the bridged shape, or the GitHub kind's declaration
// — written before Shape existed — silently starts reading the wrong
// definition.
func TestScopeLabelShape_ZeroValueIsTheBridgedShape(t *testing.T) {
	var unset ScopeLabelShape
	assert.Equal(t, ShapeNameOnResource, unset)

	b := ScopeLabelBridge{ScopeDefinition: "github_repo", BridgeDefinition: "github_repo_url", BridgeRelation: "repo"}
	assert.Equal(t, "github_repo_url", b.readDefinition(), "an undeclared Shape reads exactly as it always did")
}

// End to end through the joiner, in the stored-name shape: the name a sync
// wrote becomes the row's title, and — the part that distinguishes this
// decoder from the URL one — it is never a link, whatever the name says.
func TestScopeLabelJoiner_NamesAScopeFromAStoredLabelAndNeverLinksIt(t *testing.T) {
	j := newScopeLabelJoiner(channelLabelBridge(), map[string]bool{"C0123": true, "C0456": true})

	j.consider("C0123", "", labelID("demo-channel"))
	j.consider("C0456", "", labelID("https://evil.example"))
	j.consider("C0789", "", labelID("not-rendered"))

	got := j.resolved()
	require.Len(t, got, 2, "a label for a scope nobody is rendering must not be retained")
	assert.Equal(t, ScopeLabel{Title: "demo-channel"}, got["C0123"])
	assert.Equal(t, ScopeLabel{Title: "https://evil.example"}, got["C0456"],
		"a channel named after a URL is shown as text; an href here would be a link somebody else chose the target of")
}

// The anti-spoof guarantee must survive a WRONG DECLARATION, because Shape and
// Decoder are independent fields and nothing stops a future kind pairing them
// badly.
//
// This bridge is the mistake, written out deliberately: the stored-name shape
// with the URL decoder. Without the shape-derived guard in resolved(), a
// channel a person named `https://evil.example/a/b` renders as anchor text
// `a/b` pointing at evil.example — an attacker-chosen label on an
// attacker-chosen target, which is the whole shape of a spoofed link and
// exactly what splitting the decoders was meant to prevent.
//
// Pinning the pairing in each kind's own tests would make the guarantee hold by
// everyone remembering. Deciding it from the shape makes it hold because a
// name-on-subject payload is display text by construction, and no declaration
// can opt out.
func TestScopeLabelJoiner_AStoredNameIsNeverLinkedEvenUnderTheWrongDecoder(t *testing.T) {
	misdeclared := ScopeLabelBridge{
		ScopeDefinition:  "slack_channel",
		BridgeDefinition: "string",
		BridgeRelation:   "label",
		Shape:            ShapeNameOnSubject,
		Decoder:          resourcedisplay.DecoderB64URL, // the mistake
	}
	j := newScopeLabelJoiner(misdeclared, map[string]bool{"C0123": true})

	j.consider("C0123", "", labelID("https://evil.example/a/b"))

	got := j.resolved()
	require.Contains(t, got, "C0123", "the row still renders — this is not about hiding it")
	assert.Empty(t, got["C0123"].Href,
		"a stored name must never become a link target, whatever decoder the declaration named")
}

// The other half of the same guard, so it cannot be "fixed" by blanking every
// href: the BRIDGED shape keeps its link. Those ids are URLs this repo minted
// from a forge's canonical fields, which is the case a link is actually for.
func TestScopeLabelJoiner_TheBridgedShapeKeepsItsLink(t *testing.T) {
	j := newScopeLabelJoiner(repoBridge(), map[string]bool{"1005857813": true})

	j.consider("1005857813", "", bridgeID("https://github.com/demo-org/widgets"))

	assert.Equal(t, "https://github.com/demo-org/widgets", j.resolved()["1005857813"].Href,
		"the shape-derived guard must not blank a link the bridged shape legitimately has")
}

// A label whose subject is a USERSET is not the tuple the sync writes. The
// guard is shape-independent and has to stay that way: it is what stops a
// relation this sync does not claim from deciding what a console row is
// called.
func TestScopeLabelJoiner_IgnoresUsersetSubjectsUnderTheStoredNameShape(t *testing.T) {
	j := newScopeLabelJoiner(channelLabelBridge(), map[string]bool{"C0123": true})

	j.consider("C0123", "user", labelID("impostor"))

	assert.Empty(t, j.resolved(), "only a bare string subject names a row")
}

// A stored label that does not decode leaves the scope ABSENT from the result,
// so the caller renders the raw id — the same fallback the bridged shape has,
// and the reason an unnameable row is not an error.
func TestScopeLabelJoiner_UndecodableStoredLabelFallsBackToTheRawID(t *testing.T) {
	j := newScopeLabelJoiner(channelLabelBridge(), map[string]bool{"C0123": true})

	j.consider("C0123", "", "not base64!")

	assert.NotContains(t, j.resolved(), "C0123")
}
