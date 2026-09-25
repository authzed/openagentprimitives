package userprofile

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/untrusted"
)

func TestRenderWrapsInUntrustedMarkersWithMatchingNonce(t *testing.T) {
	got := Render(Profile{DisplayName: "Dana Whitfield", Title: "Director of Support"},
		[]Field{FieldDisplayName, FieldTitle}, "a3f9")

	assert.Contains(t, got, "<"+untrusted.ProfileTag+` nonce="a3f9">`)
	assert.Contains(t, got, "</"+untrusted.ProfileTag+` nonce="a3f9">`)
	assert.Contains(t, got, "Dana Whitfield")
	assert.Contains(t, got, "Director of Support")
}

func TestRenderStatesProfilesAreSelfReported(t *testing.T) {
	got := strings.ToLower(Render(Profile{Title: "Chief Security Officer"}, []Field{FieldTitle}, "n1"))
	assert.Contains(t, got, "self-reported",
		"a claimed title must never read as standing; the block itself has to say so")
	assert.Contains(t, got, "permission",
		"the block must state that profile data grants no permission")
}

func TestRenderScopesTheBlockToItsOwnMessageSenderNotTheCurrentSpeaker(t *testing.T) {
	got := strings.ToLower(Render(Profile{Title: "Director of Support"}, []Field{FieldTitle}, "n1"))
	assert.Contains(t, got, "the person who sent this message",
		"blocks are memoized and pinned to a person's FIRST message, so the wording must describe that "+
			"message's author, not whoever is currently speaking")
	assert.NotContains(t, got, "currently speaking",
		"the block must never claim to describe the current speaker — in a multi-participant thread "+
			"several blocks are visible at once, each scoped to a different message")
}

func TestRenderEmptyProfileYieldsNothing(t *testing.T) {
	cases := []struct {
		name  string
		p     Profile
		allow []Field
	}{
		{name: "zero profile", p: Profile{}, allow: AllFields()},
		{name: "empty allowlist", p: Profile{Title: "Director"}, allow: nil},
		{name: "allowlisted fields all empty", p: Profile{Title: ""}, allow: []Field{FieldTitle}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, Render(tc.p, tc.allow, "n1"),
				"an empty block must not be emitted — it costs tokens and says nothing")
		})
	}
}

func TestRenderOmitsNonAllowlistedFieldsEvenWhenPopulated(t *testing.T) {
	got := Render(fullProfile(), []Field{FieldTitle}, "n1")
	assert.Contains(t, got, "Director of Support")
	assert.NotContains(t, got, "dana@example.com", "email was not allowlisted")
	assert.NotContains(t, got, "+1-555-0100", "phone was not allowlisted")
}

func TestRenderIsDeterministic(t *testing.T) {
	a := Render(fullProfile(), AllFields(), "n1")
	b := Render(fullProfile(), AllFields(), "n1")
	require.Equal(t, a, b, "field order must be stable so the block is prompt-cache friendly")
}
