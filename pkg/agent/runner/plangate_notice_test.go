package runner

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
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

// A slot move on a resource line gets its own revocation line on the wire, so
// a surface cannot render the move while dropping what it costs.
func TestPlanGateItems_MoveCarriesARevocationLine(t *testing.T) {
	items := planGateItems(plangate.Card{Phases: []plangate.CardPhase{{
		Title:     "Phase 1",
		Resources: []plangate.CardLine{{Text: "git_repo", Detail: "repoB", MovedFrom: "repoA"}},
	}}})
	require.Len(t, items, 1)
	require.Len(t, items[0].Items, 2, "the resource line, then its revocation")
	assert.Equal(t, "repoB", items[0].Items[0].Detail)
	assert.Contains(t, items[0].Items[1].Text, "revokes this session's access to repoA")
	assert.Equal(t, channelevents.ToneExternal, items[0].Items[1].Tone)
}

// A `rebind: never` slot pinned elsewhere carries a BLOCKED line over the wire:
// the resource line, then its own line saying the named instance cannot be
// granted and the new-session route — external tone so a surface cannot render
// the resource above as grantable.
func TestPlanGateItems_PinnedImmovableCarriesABlockedLine(t *testing.T) {
	items := planGateItems(plangate.Card{Phases: []plangate.CardPhase{{
		Title:     "Phase 1",
		Resources: []plangate.CardLine{{Text: "git_repo", Detail: "repoB", PinnedImmovable: "repoA"}},
	}}})
	require.Len(t, items, 1)
	require.Len(t, items[0].Items, 2, "the resource line, then its blocked notice")
	assert.Equal(t, "repoB", items[0].Items[0].Detail)
	assert.Contains(t, items[0].Items[1].Text, "cannot be granted")
	assert.Contains(t, items[0].Items[1].Text, "pinned to repoA")
	assert.Contains(t, items[0].Items[1].Text, "start a new session to reach repoB")
	assert.Equal(t, channelevents.ToneExternal, items[0].Items[1].Tone)
}

// The approver gets ONE notice per approval, and it must be true for every type
// the approval covered: GrantSlots resolves each type on its own, so one call can
// bind one type, refuse another and blip on a third. Each case folds one or more
// phases' outcomes, the way recordPlanGateDecision does, and checks the copy.
func TestApprovalOutcome_Notice(t *testing.T) {
	refused := func(rt, rebind string) *authz.SlotBindFailure {
		return authz.NewSlotBindFailure(authz.SlotFailureRefused, rebind,
			fmt.Errorf("%w: this session is pinned to %s:A", authz.ErrSlotPinned, rt), rt)
	}
	fault := func(rt string) *authz.SlotBindFailure {
		return authz.NewSlotBindFailure(authz.SlotFailureFault, "",
			fmt.Errorf("%w: ensure pin: dial tcp 10.0.0.1:50051: Unavailable", authz.ErrSlotStore), rt)
	}
	committed := func(rt string) *authz.SlotBindFailure {
		return authz.NewSlotBindFailure(authz.SlotFailureMoveCommitted, "",
			fmt.Errorf("%w: write failed", authz.ErrSlotMoveCommitted), rt)
	}
	drifted := func(rt string) *authz.SlotBindFailure {
		return authz.NewSlotBindFailure(authz.SlotFailureDrifted, "",
			fmt.Errorf("%w: %w", authz.ErrSlotMoveDrifted, authz.ErrSlotPinned), rt)
	}
	type phase struct {
		types    []string
		failures []*authz.SlotBindFailure
	}
	bindingsOf := func(types ...string) []authz.SlotBinding {
		out := make([]authz.SlotBinding, 0, len(types))
		for _, rt := range types {
			out = append(out, authz.SlotBinding{ResourceType: rt})
		}
		return out
	}
	cases := []struct {
		name         string
		phases       []phase
		wantNotice   bool
		wantCategory string
		leadHas      []string
		leadNot      []string
		nextHas      []string
		nextNot      []string
	}{
		{name: "every type bound: no notice",
			phases: []phase{{types: []string{"git_repo", "label"}}}},
		{name: "partial store fault: names the faulted type and never claims nothing was granted",
			phases:     []phase{{types: []string{"git_repo", "label"}, failures: []*authz.SlotBindFailure{fault("label")}}},
			wantNotice: true, wantCategory: categories.InternalError,
			leadHas: []string{"could not be fully applied", "label", "fault on our side"},
			leadNot: []string{"nothing was granted", "git_repo", "dial tcp"},
			nextHas: []string{"ask again"}, nextNot: []string{"nothing has been granted"}},
		{name: "drifted move: a fault, never a refusal",
			phases:     []phase{{types: []string{"git_repo"}, failures: []*authz.SlotBindFailure{drifted("git_repo")}}},
			wantNotice: true, wantCategory: categories.InternalError,
			leadHas: []string{"fault on our side"}, leadNot: []string{"already committed"}},
		{name: "store fault beside a refusal: refusal copy, the fault named separately, no store text",
			phases: []phase{{types: []string{"git_repo", "label"},
				failures: []*authz.SlotBindFailure{refused("git_repo", ""), fault("label")}}},
			wantNotice: true, wantCategory: categories.SlotBindRefused,
			leadHas: []string{"already committed to a different target for git_repo", "Other instances"},
			leadNot: []string{"dial tcp"},
			nextHas: []string{"approved amendment moves the commitment", "Separately, access to label", "fault on our side"},
			nextNot: []string{"dial tcp"}},
		{name: "refused rebind:never beside a bound default type: new session only",
			phases: []phase{{types: []string{"ticket", "git_repo"},
				failures: []*authz.SlotBindFailure{refused("git_repo", authz.SlotRebindNever)}}},
			wantNotice: true, wantCategory: categories.SlotBindRefused,
			nextHas: []string{"cannot be moved", "Start a new session"}, nextNot: []string{"amendment"}},
		{name: "refused never and refused movable: the amendment route stays open",
			phases: []phase{{types: []string{"ticket", "git_repo"},
				failures: []*authz.SlotBindFailure{refused("git_repo", authz.SlotRebindNever), refused("ticket", "")}}},
			wantNotice: true, wantCategory: categories.SlotBindRefused,
			nextHas: []string{"approved amendment"}},
		{name: "committed move with a fault elsewhere: says what was revoked, names the fault",
			phases: []phase{{types: []string{"git_repo", "label"},
				failures: []*authz.SlotBindFailure{committed("git_repo"), fault("label")}}},
			wantNotice: true, wantCategory: categories.InternalError,
			leadHas: []string{"moved this session to the new target for git_repo", "removed its access"},
			nextHas: []string{"Separately, access to label"}},
		{name: "a later phase's bind supersedes an earlier committed move: no notice",
			phases: []phase{
				{types: []string{"git_repo"}, failures: []*authz.SlotBindFailure{committed("git_repo")}},
				{types: []string{"git_repo"}},
			}},
		{name: "a later phase's bind supersedes an earlier fault: no notice",
			phases: []phase{
				{types: []string{"git_repo"}, failures: []*authz.SlotBindFailure{fault("git_repo")}},
				{types: []string{"git_repo"}},
			}},
		{name: "a refusal is sticky across a later phase's bind",
			phases: []phase{
				{types: []string{"git_repo"}, failures: []*authz.SlotBindFailure{refused("git_repo", "")}},
				{types: []string{"git_repo"}},
			},
			wantNotice: true, wantCategory: categories.SlotBindRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var o approvalOutcome
			for _, ph := range tc.phases {
				var err error
				if len(ph.failures) > 0 {
					errs := make([]error, 0, len(ph.failures))
					for _, f := range ph.failures {
						errs = append(errs, f)
					}
					err = fmt.Errorf("bindSlots(approved): grant slots: %w", errors.Join(errs...))
				}
				require.True(t, o.record(bindingsOf(ph.types...), err))
			}
			n, ok := o.notice()
			require.Equal(t, tc.wantNotice, ok)
			if !ok {
				return
			}
			assert.Equal(t, tc.wantCategory, n.category)
			for _, want := range tc.leadHas {
				assert.Contains(t, n.lead, want)
			}
			for _, not := range tc.leadNot {
				assert.NotContains(t, n.lead, not)
			}
			for _, want := range tc.nextHas {
				assert.Contains(t, n.nextStep, want)
			}
			for _, not := range tc.nextNot {
				assert.NotContains(t, n.nextStep, not)
			}
		})
	}
}

// An error raised before GrantSlots ran (a session_scope write) carries no
// per-type outcome; record says so and the caller keeps it log-only.
func TestApprovalOutcome_RecordRejectsANonSlotError(t *testing.T) {
	var o approvalOutcome
	assert.False(t, o.record([]authz.SlotBinding{{ResourceType: "git_repo"}}, errors.New("write session_scope: conflict")))
	_, ok := o.notice()
	assert.False(t, ok)
}

// Two phases moving one pin to two different instances is detected before any
// write; the same move repeated across phases (idempotent at the target) and a
// multi-occupancy type are not conflicts.
func TestConflictingMoveTargets(t *testing.T) {
	move := func(rt, id, prior string) authz.SlotBinding {
		return authz.SlotBinding{ResourceType: rt, ResourceID: authz.TrustedObjectID(id), PriorID: authz.TrustedObjectID(prior)}
	}
	assert.Equal(t, map[string][]string{"git_repo": {"repoB", "repoC"}},
		conflictingMoveTargets([][]authz.SlotBinding{{move("git_repo", "repoB", "repoA")}, {move("git_repo", "repoC", "repoA")}}))
	assert.Nil(t, conflictingMoveTargets([][]authz.SlotBinding{{move("git_repo", "repoB", "repoA")}, {move("git_repo", "repoB", "repoA")}}),
		"the same move in every phase is one move")
	multi := move("label", "b", "a")
	multi.Occupancy = authz.SlotOccupancyMulti
	multi2 := move("label", "c", "a")
	multi2.Occupancy = authz.SlotOccupancyMulti
	assert.Nil(t, conflictingMoveTargets([][]authz.SlotBinding{{multi}, {multi2}}), "a multi-occupancy slot holds no pin to conflict over")
	assert.Nil(t, conflictingMoveTargets([][]authz.SlotBinding{{
		{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("repoB")},
		{ResourceType: "git_repo", ResourceID: authz.TrustedObjectID("repoC")},
	}}), "first-fills are not moves; GrantSlots refuses that per type")
}
