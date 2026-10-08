package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// planWithPushPhaseAndRepo is planWithPushPhase, plus a git_repo slot naming
// an instance — for tests that care about how a RESOURCE line renders.
func planWithPushPhaseAndRepo(t *testing.T, repoURL string) Plan {
	t.Helper()
	p := planWithPushPhase(t)
	p.Phases[0].Slots = []Slot{{Type: "git_repo", ID: repoURL}}
	return p
}

// A declared display resolves onto the resource line: a derived label as
// Text, a known icon, and — because the raw value is an eligible https URL —
// an Href identical to Detail.
func TestBuildCard_ResourceLineCarriesDeclaredDisplay(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:      planWithPushPhaseAndRepo(t, "https://github.com/demo-org/demo-repo"),
		WholePlan: true,
		Surface:   []permsurface.Descriptor{permDesc(t, "push", "git_repo", "")},
		ResourceDisplays: map[string]ResourceDisplay{
			"git_repo": {Name: "Git repository", Icon: "repository", Label: "url_path"},
		},
	})

	require.Len(t, card.Phases, 1)
	require.Len(t, card.Phases[0].Resources, 1)
	line := card.Phases[0].Resources[0]
	assert.Equal(t, "demo-org/demo-repo", line.Text, "the derived label reaches the card, not the wire type name")
	assert.Equal(t, "https://github.com/demo-org/demo-repo", line.Detail, "the canonical value is always adjacent")
	assert.Equal(t, "repository", line.Icon)
	assert.Equal(t, "https://github.com/demo-org/demo-repo", line.Href,
		"an eligible https instance value gets a real link")
	assert.Equal(t, line.Detail, line.Href, "text == href: the two must be byte-identical")
}

// A LOOKALIKE host derives the identical label as the real one — the
// canonical value is the only thing that differs, and it must still be
// present and still be the actual link target. This is invariant 3 made
// concrete: shortening alone would make the lookalike indistinguishable from
// the real target on the one card that must be trustworthy.
func TestBuildCard_ResourceLine_LookalikeHostDerivesIdenticalLabelButDifferentHref(t *testing.T) {
	displays := map[string]ResourceDisplay{"git_repo": {Label: "url_path"}}

	real := BuildCard(CardInput{
		Plan: planWithPushPhaseAndRepo(t, "https://github.com/demo-org/demo-repo"), WholePlan: true,
		ResourceDisplays: displays,
	})
	lookalike := BuildCard(CardInput{
		Plan: planWithPushPhaseAndRepo(t, "https://github.co/demo-org/demo-repo"), WholePlan: true,
		ResourceDisplays: displays,
	})

	realLine := real.Phases[0].Resources[0]
	lookalikeLine := lookalike.Phases[0].Resources[0]
	assert.Equal(t, realLine.Text, lookalikeLine.Text, "the derived label alone cannot tell them apart")
	assert.NotEqual(t, realLine.Detail, lookalikeLine.Detail, "the canonical value is what actually differs")
	assert.Equal(t, "https://github.com/demo-org/demo-repo", realLine.Href)
	assert.Equal(t, "https://github.co/demo-org/demo-repo", lookalikeLine.Href)
}

// With no Display declared for the resource type, behavior is EXACTLY what
// it was before this feature existed: the wire type name as Text, no icon,
// no href — this feature is additive and opt-in per type.
func TestBuildCard_ResourceLine_NoDisplayDeclaredFallsBackToWireTypeName(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:      planWithPushPhaseAndRepo(t, "https://github.com/demo-org/demo-repo"),
		WholePlan: true,
	})

	line := card.Phases[0].Resources[0]
	assert.Equal(t, "git_repo", line.Text)
	assert.Equal(t, "https://github.com/demo-org/demo-repo", line.Detail)
	assert.Empty(t, line.Icon)
	assert.Empty(t, line.Href, "no Display declared: the pre-existing behavior renders no link either")
}

// A declared deriver that produces nothing for THIS instance (no path) falls
// back to the type's declared Name rather than the wire type name.
func TestBuildCard_ResourceLine_DeriverProducesNothingFallsBackToName(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:      planWithPushPhaseAndRepo(t, "https://github.com"),
		WholePlan: true,
		ResourceDisplays: map[string]ResourceDisplay{
			"git_repo": {Name: "Git repository", Label: "url_path"},
		},
	})

	line := card.Phases[0].Resources[0]
	assert.Equal(t, "Git repository", line.Text)
}

// An unrecognized icon name never reaches the card — no fallback image, no
// guess, exactly what knownIcon enforces, checked here at the BuildCard
// boundary rather than just at the registry's own unit test.
func TestBuildCard_ResourceLine_UnrecognizedIconRendersNothing(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:      planWithPushPhaseAndRepo(t, "https://github.com/demo-org/demo-repo"),
		WholePlan: true,
		ResourceDisplays: map[string]ResourceDisplay{
			"git_repo": {Icon: "not-a-real-icon"},
		},
	})

	assert.Empty(t, card.Phases[0].Resources[0].Icon)
}

// A non-https instance value derives a label just fine but gets no link —
// the security floor applies independently of how legible the label is.
func TestBuildCard_ResourceLine_NonHTTPSValueGetsNoHref(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:      planWithPushPhaseAndRepo(t, "git@github.com:demo-org/demo-repo.git"),
		WholePlan: true,
		ResourceDisplays: map[string]ResourceDisplay{
			"git_repo": {Name: "Git repository", Label: "url_path"},
		},
	})

	line := card.Phases[0].Resources[0]
	assert.Empty(t, line.Href, "an SSH-style remote is not an eligible link")
	assert.Equal(t, "Git repository", line.Text, "no derivable path: falls back to the declared Name")
}

// A phase's CEILING can name an instance the agent never declared as a slot.
// git.yaml keys git_repo read/write on the literal "workspace" — the session's
// own checked-out copy — while the declared slot names the remote URL. A card
// listing only the declared slot tells an approver the phase reaches one
// repository when it will also read and write a second, different object, and
// the approval they give does not cover that one.
//
// Observed live 2026-08-21: a card read "reaches demo-org/demo-repo" for a phase
// whose ceiling included "Read the checked-out files and history", and the
// runner then logged `does not have read on git_repo:workspace` — an instance
// that appeared nowhere on the card the human approved.
func TestBuildCard_ResourceLines_IncludeConstantInstancesFromTheCeiling(t *testing.T) {
	p := planWithPushPhaseAndRepo(t, "https://github.com/acme/widgets")
	p.Phases[0].Permissions = append(p.Phases[0].Permissions, handle(t, "read", "git_repo"))

	readDesc := permDesc(t, "read", "git_repo", authz.Readonly)
	readDesc.ConstantResourceID = "workspace"

	card := BuildCard(CardInput{
		Plan:      p,
		WholePlan: true,
		Surface:   []permsurface.Descriptor{permDesc(t, "push", "git_repo", authz.External), readDesc},
	})

	require.Len(t, card.Phases, 1)
	var details []string
	for _, r := range card.Phases[0].Resources {
		details = append(details, r.Detail)
	}
	assert.Contains(t, details, "https://github.com/acme/widgets", "the agent's declared slot instance")
	assert.Contains(t, details, "workspace", "the constant instance the ceiling reaches, which no slot declared")
}

// A constant that names the SAME instance a slot already declared is listed
// once. The two arrive by different routes — one computed from the ceiling,
// one declared by the agent — and printing both would read as two distinct
// targets on the one surface where a human counts what they are approving.
func TestBuildCard_ResourceLines_ConstantMatchingADeclaredSlotIsNotDuplicated(t *testing.T) {
	p := planWithPushPhaseAndRepo(t, "workspace")

	pushDesc := permDesc(t, "push", "git_repo", authz.External)
	pushDesc.ConstantResourceID = "workspace"

	card := BuildCard(CardInput{
		Plan: p, WholePlan: true,
		Surface: []permsurface.Descriptor{pushDesc},
	})

	require.Len(t, card.Phases, 1)
	var n int
	for _, r := range card.Phases[0].Resources {
		if r.Detail == "workspace" {
			n++
		}
	}
	assert.Equal(t, 1, n, "one instance, one line, however many routes name it")
}

// An AMENDMENT must name the instance its added permission reaches, exactly as
// a phase card does. The added permission can key on a CONSTANT the agent
// cannot name — git_repo read/write key on the literal "workspace" — so an
// amendment listing only the permission tells an approver what authority is
// being added without telling them what it touches.
func TestBuildCard_Amendment_NamesTheConstantInstance(t *testing.T) {
	readDesc := permDesc(t, "read", "git_repo", authz.Readonly)
	readDesc.ConstantResourceID = "workspace"

	card := BuildCard(CardInput{
		Plan:         planWithPushPhase(t),
		PhaseIndex:   0,
		AddedHandles: []string{handle(t, "read", "git_repo").String()},
		Surface:      []permsurface.Descriptor{permDesc(t, "push", "git_repo", authz.External), readDesc},
	})

	assert.Contains(t, card.What, "workspace",
		"the amendment must name the instance its added permission reaches, not just the permission")
}

// One repository reached through two toolkits is ONE line.
//
// git names a repository by its remote URL and gh names it by OWNER/NAME, so
// a phase that clones and then opens a pull request declares two slots of two
// TYPES whose raw values do not match as strings. Rendered naively that is two
// resource lines — a reviewer reading the card counts two targets, one of them
// wearing a generic icon and the other GitHub's, for a single repository.
//
// The slots' declared value chains are what settle it: both run
// github_repo_id, so both mint the same object id, and the same id means the
// same instance however many types route to it.
func TestBuildCard_OneRepositoryThroughTwoToolkitsRendersOneLine(t *testing.T) {
	p := planWithPushPhase(t)
	p.Phases[0].Slots = []Slot{
		{Type: "git_repo", ID: "https://github.com/demo-org/demo-repo.git"},
		{Type: "github_repo", ID: "demo-org/demo-repo"},
	}

	card := BuildCard(CardInput{
		Plan:      p,
		WholePlan: true,
		Surface:   []permsurface.Descriptor{permDesc(t, "push", "git_repo", "")},
		ResourceDisplays: map[string]ResourceDisplay{
			"git_repo":    {Name: "Git repository", Icon: "repository", Label: "url_path"},
			"github_repo": {Name: "GitHub repository", Icon: "github", Label: "url_path"},
		},
		SlotValueTransforms: map[string][]string{
			"git_repo":    {"github_repo_id", "normalize_url", "spicedb_escape"},
			"github_repo": {"github_repo_id", "spicedb_escape"},
		},
	})

	require.Len(t, card.Phases, 1)
	require.Len(t, card.Phases[0].Resources, 1,
		"one repository reached two ways is one target; got %d lines", len(card.Phases[0].Resources))
	assert.Equal(t, "demo-org/demo-repo", card.Phases[0].Resources[0].Text)
}

// Two DIFFERENT repositories stay two lines. The dedup keys on the derived id,
// so it can only ever merge instances that are genuinely the same one — the
// property that keeps it from hiding a target from an approver.
func TestBuildCard_DifferentRepositoriesStayDistinctLines(t *testing.T) {
	p := planWithPushPhase(t)
	p.Phases[0].Slots = []Slot{
		{Type: "git_repo", ID: "https://github.com/demo-org/demo-repo"},
		{Type: "github_repo", ID: "demo-org/other-repo"},
	}

	card := BuildCard(CardInput{
		Plan: p, WholePlan: true,
		Surface: []permsurface.Descriptor{permDesc(t, "push", "git_repo", "")},
		ResourceDisplays: map[string]ResourceDisplay{
			"git_repo":    {Label: "url_path"},
			"github_repo": {Label: "url_path"},
		},
		SlotValueTransforms: map[string][]string{
			"git_repo":    {"github_repo_id", "normalize_url", "spicedb_escape"},
			"github_repo": {"github_repo_id", "spicedb_escape"},
		},
	})

	require.Len(t, card.Phases, 1)
	assert.Len(t, card.Phases[0].Resources, 2, "two repositories are two targets")
}

// With no declared chains — a class that publishes no ResolvedSlots, or a type
// with no transforms — the raw values are compared as they always were. The
// dedup must never merge on a guess.
func TestBuildCard_WithoutValueTransforms_RawValuesDecide(t *testing.T) {
	p := planWithPushPhase(t)
	p.Phases[0].Slots = []Slot{
		{Type: "git_repo", ID: "https://github.com/demo-org/demo-repo"},
		{Type: "github_repo", ID: "demo-org/demo-repo"},
	}

	card := BuildCard(CardInput{
		Plan: p, WholePlan: true,
		Surface:          []permsurface.Descriptor{permDesc(t, "push", "git_repo", "")},
		ResourceDisplays: map[string]ResourceDisplay{"git_repo": {Label: "url_path"}},
	})

	require.Len(t, card.Phases, 1)
	assert.Len(t, card.Phases[0].Resources, 2,
		"different raw strings and no chain to canonicalize them: two lines, as before")
}

// An amendment card names a resource the way a human reads it, not the way
// SpiceDB stores it.
//
// buildSlotSection wrote the raw wire type name while the phase card beside it
// resolved the declared display. An approver saw "git_repo — workspace" on one
// card and "Git repository workspace" on the other, for the same thing — and
// the wire name is an internal identifier that has no business on a surface a
// person decides from.
func TestBuildCard_AmendmentSlotLineUsesTheDeclaredDisplayName(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:         planWithPushPhase(t),
		PhaseIndex:   0,
		AddedHandles: []string{"perm:read:git_repo"},
		AddedSlots:   []Slot{{Type: "git_repo", ID: "workspace"}},
		// Titles present, as they are live: without them the handle line falls
		// back to a detokenized form and the assertions below would be reading
		// that instead of the slot line under test.
		PermissionTitles: map[string]string{"git_repo/read": "Read the checked-out files and history"},
		Surface:          []permsurface.Descriptor{permDesc(t, "read", "git_repo", "")},
		ResourceDisplays: map[string]ResourceDisplay{
			"git_repo": {Name: "Git repository", Icon: "repository", Label: "url_path"},
		},
		SlotPermissions: map[string]string{"git_repo": "push"},
	})

	assert.Contains(t, card.What, "Git repository",
		"the declared display name is what a human reads")
	assert.NotContains(t, card.What, "git_repo",
		"the wire type name is an internal identifier and must not reach an approver")
}

// The sentence under an amendment's slot names the permission the AMENDMENT
// grants, not whatever the slot was declared for.
//
// A card adding "Read the checked-out files and history" told the approver
// "This session will be able to push for as long as it runs." — it read the
// slot's declared permission (push) instead of the one being added. That is
// the single most alarming word on the card, attached to the wrong decision.
func TestBuildCard_AmendmentSlotSentenceNamesTheAddedPermission(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:         planWithPushPhase(t),
		PhaseIndex:   0,
		AddedHandles: []string{"perm:read:git_repo"},
		AddedSlots:   []Slot{{Type: "git_repo", ID: "workspace"}},
		// Titles present, as they are live: without them the handle line falls
		// back to a detokenized form and the assertions below would be reading
		// that instead of the slot line under test.
		PermissionTitles: map[string]string{"git_repo/read": "Read the checked-out files and history"},
		Surface:          []permsurface.Descriptor{permDesc(t, "read", "git_repo", "")},
		ResourceDisplays: map[string]ResourceDisplay{
			"git_repo": {Name: "Git repository", Label: "url_path"},
		},
		// The slot is DECLARED for push; the amendment adds read.
		SlotPermissions: map[string]string{"git_repo": "push"},
	})

	assert.NotContains(t, card.What, "able to push",
		"the amendment adds read; saying push overstates what approving grants")
	assert.Contains(t, card.What, "read",
		"the sentence must name the permission actually being added")
}

// A whole-plan card renders a slot MOVE exactly as the single-phase card does:
// both instances in the prose, the revocation stated, and the displaced
// instance carried structurally on the resource line so a surface can render
// the revocation as its own line. Detail stays the new instance alone.
func TestBuildCard_WholePlanResourceLineShowsAMove(t *testing.T) {
	p := planWithPushPhaseAndRepo(t, "https://github.com/demo-org/new-repo")
	p.Phases[0].Slots[0].MovedFrom = "demo-org/old-repo"
	card := BuildCard(CardInput{Plan: p, WholePlan: true})

	assert.Contains(t, card.What, "demo-org/old-repo → https://github.com/demo-org/new-repo")
	assert.Contains(t, card.What, "revokes this session's access to demo-org/old-repo")
	require.Len(t, card.Phases, 1)
	require.Len(t, card.Phases[0].Resources, 1)
	line := card.Phases[0].Resources[0]
	assert.Equal(t, "demo-org/old-repo", line.MovedFrom)
	assert.Equal(t, "https://github.com/demo-org/new-repo", line.Detail, "Detail stays the new instance")
}
