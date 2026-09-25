package relsync_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// Blank-imported so their init()s register with relsync (and, for
	// onepassword, with relsource too) in THIS test binary. relsync cannot
	// import either directly — both import relsync — so the registration
	// this test asserts on has to be pulled in here, the same way it must be
	// pulled into any real binary (see the operator's and the e2e harness's
	// own blank imports of these two packages).
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/onepassword"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// The claim the whole registry design rests on: a second directory is a ROW,
// not a branch. Nothing outside a kind's own package may switch on its name.
func TestRegistry_TwoKindsCoexistWithDisjointClaims(t *testing.T) {
	// relsync exposes Get(name) (Kind, bool) — there is no MustGet.
	slackKind, ok := relsync.Get("slack")
	require.True(t, ok, "the slack kind must be registered")
	opKind, ok := relsync.Get("onepassword")
	require.True(t, ok, "the onepassword kind must be registered")

	slackSrc, opSrc := slackKind.Source(), opKind.Source()

	assert.NotEmpty(t, slackSrc.Claims)
	assert.NotEmpty(t, opSrc.Claims)
	for _, c := range opSrc.Claims {
		assert.NotContains(t, slackSrc.Claims, c,
			"two kinds claiming one relation is a startup panic by design; they must be disjoint")
	}
}

// A cursor minted by one kind is refused by the other — the failure that
// motivated tagging cursors in the first place.
func TestRegistry_CursorsDoNotCrossKinds(t *testing.T) {
	slackCur := relsync.Cursor{Kind: "slack", Token: "dGVhbTpDMDE5"}

	_, err := slackCur.ForKind("onepassword")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "slack")
	assert.Contains(t, err.Error(), "onepassword")
}
