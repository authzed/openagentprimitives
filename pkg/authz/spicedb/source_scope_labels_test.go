package spicedb

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
)

// These are pure-logic tests, like source_scopes_test.go's: the joiner never
// touches the network, and resolveScopeLabels' "nothing to name" branch
// returns before it would. Neither calls relsource.Register, so nothing here
// needs the "!integration && !e2e" build constraint — there is no global
// registry write to leak into the integration binary. The stream itself
// (readScopeLabels' real ReadRelationships call) is exercised in
// source_scopes_integration_test.go.

func repoBridge() ScopeLabelBridge {
	return ScopeLabelBridge{
		ScopeDefinition:  "github_repo",
		BridgeDefinition: "github_repo_url",
		BridgeRelation:   "repo",
		Decoder:          resourcedisplay.DecoderB64URL,
	}
}

// bridgeID encodes a URL the way both the GitHub sync's repoURLObjectID and
// the github_repo_url_id transform do — unpadded base64url of the canonical
// https form.
func bridgeID(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// The join is by SUBJECT object id, and only for the scopes actually being
// rendered. A bridge row for a repository that was truncated away by the cap
// costs nothing and contributes nothing.
func TestScopeLabelJoiner_JoinsOnlyWantedScopesAndNamesThem(t *testing.T) {
	j := newScopeLabelJoiner(repoBridge(), map[string]bool{"1005857813": true})

	j.consider("1005857813", "", bridgeID("https://github.com/demo-org/widgets"))
	j.consider("2000000000", "", bridgeID("https://github.com/demo-org/not-rendered"))

	got := j.resolved()
	require.Len(t, got, 1, "a bridge row for an unrendered scope must not be retained")
	assert.Equal(t, ScopeLabel{
		Title: "demo-org/widgets",
		Href:  "https://github.com/demo-org/widgets",
	}, got["1005857813"])
}

// A bridge row whose subject is a USERSET is not the bridge tuple the sync
// writes. Naming a row from one would let a relation this sync does not claim
// decide what a console row is called.
func TestScopeLabelJoiner_IgnoresUsersetSubjects(t *testing.T) {
	j := newScopeLabelJoiner(repoBridge(), map[string]bool{"1005857813": true})

	j.consider("1005857813", "member", bridgeID("https://github.com/demo-org/impostor"))

	assert.Empty(t, j.resolved(), "only a bare scope-object subject may name a row")
}

// A stale bridge edge can outlive a rename until the next full-coverage pass
// reaps it, and SpiceDB's stream order is not reproducible. Whichever order
// the two rows arrive in, the page must settle on the same name — a title that
// flips between refreshes is worse than a raw id, because it looks like data
// changing.
func TestScopeLabelJoiner_TieBreakIsOrderIndependent(t *testing.T) {
	stale := bridgeID("https://github.com/demo-org/old-name")
	fresh := bridgeID("https://github.com/demo-org/new-name")

	forward := newScopeLabelJoiner(repoBridge(), map[string]bool{"1005857813": true})
	forward.consider("1005857813", "", stale)
	forward.consider("1005857813", "", fresh)

	backward := newScopeLabelJoiner(repoBridge(), map[string]bool{"1005857813": true})
	backward.consider("1005857813", "", fresh)
	backward.consider("1005857813", "", stale)

	assert.Equal(t, forward.resolved(), backward.resolved(),
		"two arrival orders of the same rows must produce the same name")
	assert.NotEmpty(t, forward.resolved()["1005857813"].Title)
}

// The security-shaped fallback: a bridge id this repo did not mint must
// degrade to ABSENT, so the caller renders the forge id. Not a truncated
// title, not a decoded blob, not a link — absent.
func TestScopeLabelJoiner_MalformedBridgeIDDegradesToAbsent(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "not base64 at all", raw: "!!!not-base64!!!"},
		{name: "base64 of something that is not a URL", raw: bridgeID("just some bytes")},
		{name: "base64 of a URL with no path to name", raw: bridgeID("https://github.com")},
		{name: "base64 of a hostile scheme", raw: bridgeID("javascript:alert(1)")},
		{name: "empty", raw: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := newScopeLabelJoiner(repoBridge(), map[string]bool{"1005857813": true})
			j.consider("1005857813", "", tc.raw)
			got := j.resolved()
			assert.NotContains(t, got, "1005857813",
				"an id that does not decode must be absent, so the caller falls back to the raw id")
		})
	}
}

// An http bridge id names a row but must never LINK it — the scheme guard has
// to survive the decode, because the decoded value is what becomes the href.
func TestScopeLabelJoiner_NamesButDoesNotLinkANonHTTPSValue(t *testing.T) {
	j := newScopeLabelJoiner(repoBridge(), map[string]bool{"1005857813": true})
	j.consider("1005857813", "", bridgeID("http://ghe.internal.example/demo-org/widgets"))

	got := j.resolved()
	require.Contains(t, got, "1005857813")
	assert.Equal(t, "demo-org/widgets", got["1005857813"].Title)
	assert.Empty(t, got["1005857813"].Href, "only https is ever rendered as a link target")
}

// A bridge for a definition that synced nothing must not be read at all: no
// round trip, and — crucially — no "unavailable" notice on a page with
// nothing missing from it. Proven with a zero-value *Client: any read attempt
// would nil-panic on the embedded gRPC client.
func TestResolveScopeLabels_SkipsABridgeWithNoRowsToName(t *testing.T) {
	c := &Client{}

	labels, unavailable := c.resolveScopeLabels(context.Background(),
		[]SourceScope{{Definition: "slack_channel", ScopeIDs: []string{"C0123"}, Total: 1}},
		[]ScopeLabelBridge{repoBridge()}, "GitHub")

	assert.Empty(t, labels)
	assert.Empty(t, unavailable, "a bridge nobody needed must not raise a notice")
}

// The no-bridges case — every kind that declares no ScopeLabeler. Same proof
// shape: a zero-value *Client cannot read, so reaching this cleanly means
// nothing was read.
func TestResolveScopeLabels_NoBridgesIsNotAFailure(t *testing.T) {
	c := &Client{}

	labels, unavailable := c.resolveScopeLabels(context.Background(),
		[]SourceScope{{Definition: "github_repo", ScopeIDs: []string{"1005857813"}, Total: 1}},
		nil, "GitHub")

	assert.Empty(t, labels)
	assert.Empty(t, unavailable)
}
