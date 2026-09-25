package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// A read and an irreversible push must not look identical on the card: the
// tier a permission line carries has to survive the translation from the
// card's own vocabulary (CardLine.Impact) onto the wire's (InteractionItem.Tone),
// and the raw handle has to ride along as a hint a surface may reveal on
// demand — never as text a reader is shown by default.
func TestPlanGateItems_CarryTierAndHint(t *testing.T) {
	items := planGateItems(plangate.Card{Phases: []plangate.CardPhase{{
		Title: "Phase 1",
		Permissions: []plangate.CardLine{
			{Text: "Read the repository", Impact: "readonly", Handle: "perm:read:git_repo"},
			{Text: "Push commits", Impact: "external", Handle: "perm:push:git_repo", External: true},
		},
	}}})

	require.Len(t, items, 1)
	lines := items[0].Items
	require.Len(t, lines, 2)
	assert.Equal(t, channelevents.ToneReadonly, lines[0].Tone, "readonly Impact must carry the readonly tone")
	assert.Equal(t, "perm:read:git_repo", lines[0].Hint, "the handle rides as hover text, never as visible text")
	assert.Equal(t, channelevents.ToneExternal, lines[1].Tone, "external Impact must carry the external tone")
	assert.NotContains(t, lines[1].Text, "perm:", "the handle must never be concatenated into the visible line")
	assert.Equal(t, "perm:push:git_repo", lines[1].Hint, "an external line's handle rides as hint too")
}

// readwrite is the third tier and gets no special-casing beside the other
// two — a permission that mutates in-session state but never leaves it.
func TestPlanGateItems_ReadwriteImpactCarriesReadwriteTone(t *testing.T) {
	items := planGateItems(plangate.Card{Phases: []plangate.CardPhase{{
		Title: "Phase 1",
		Permissions: []plangate.CardLine{
			{Text: "Update the issue", Impact: "readwrite", Handle: "perm:write:issue"},
		},
	}}})

	require.Len(t, items, 1)
	require.Len(t, items[0].Items, 1)
	assert.Equal(t, channelevents.ToneReadwrite, items[0].Items[0].Tone)
}

// A CardLine with no Impact (the resource lines under a phase, which never set
// it) must not fabricate a tone: an empty Impact means the translation has
// nothing to carry, not that the resource is readonly by default.
func TestPlanGateItems_NoImpactCarriesNoTone(t *testing.T) {
	items := planGateItems(plangate.Card{Phases: []plangate.CardPhase{{
		Title: "Phase 1",
		Permissions: []plangate.CardLine{
			{Text: "Untiered line", Handle: "perm:noop:thing"},
		},
	}}})

	require.Len(t, items, 1)
	require.Len(t, items[0].Items, 1)
	assert.Empty(t, items[0].Items[0].Tone, "no Impact must not be guessed into a tone")
}

// Pins the external badge's wording so it cannot drift from the prose sites in
// pkg/authz/plangate/card.go, which state the same fact in the same words for
// surfaces with no structure to render.
func TestPlanGateItems_ExternalLineDetailReadsLeavesThisSession(t *testing.T) {
	items := planGateItems(plangate.Card{Phases: []plangate.CardPhase{{
		Title: "Phase 1",
		Permissions: []plangate.CardLine{
			{Text: "Push commits", Impact: "external", Handle: "perm:push:git_repo", External: true},
		},
	}}})

	require.Len(t, items, 1)
	require.Len(t, items[0].Items, 1)
	assert.Equal(t, "leaves this session", items[0].Items[0].Detail)
}

// A resource line's Icon and Href ride straight over the wire — the runner
// makes no decision here, it only translates the card's own vocabulary
// (CardLine) onto the wire's (InteractionItem), exactly like Impact->Tone
// above.
func TestPlanGateItems_ResourceLineCarriesIconAndHref(t *testing.T) {
	items := planGateItems(plangate.Card{Phases: []plangate.CardPhase{{
		Title: "Phase 1",
		Resources: []plangate.CardLine{
			{
				Text: "demo-org/demo-repo", Detail: "https://github.com/demo-org/demo-repo",
				Icon: "repository", Href: "https://github.com/demo-org/demo-repo",
			},
		},
	}}})

	require.Len(t, items, 1)
	require.Len(t, items[0].Items, 1)
	line := items[0].Items[0]
	assert.Equal(t, "reaches demo-org/demo-repo", line.Text)
	assert.Equal(t, "https://github.com/demo-org/demo-repo", line.Detail)
	assert.Equal(t, "repository", line.Icon)
	assert.Equal(t, "https://github.com/demo-org/demo-repo", line.Href)
	assert.Equal(t, line.Detail, line.Href, "text == href: the wire line must carry them byte-identical")
}

// A resource line with no eligible link (CardLine.Href empty) must not
// fabricate one — the InteractionItem's Href stays empty and a surface
// renders Detail as plain text.
func TestPlanGateItems_ResourceLineWithNoHrefCarriesNone(t *testing.T) {
	items := planGateItems(plangate.Card{Phases: []plangate.CardPhase{{
		Title:     "Phase 1",
		Resources: []plangate.CardLine{{Text: "git_repo", Detail: "(no target named yet)"}},
	}}})

	require.Len(t, items, 1)
	require.Len(t, items[0].Items, 1)
	assert.Empty(t, items[0].Items[0].Href)
	assert.Empty(t, items[0].Items[0].Icon)
}
