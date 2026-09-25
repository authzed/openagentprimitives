package plangate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

func planWithWhy(t *testing.T, why string) Plan {
	t.Helper()
	return Plan{Phases: []Phase{{
		Why:         why,
		Permissions: []permsurface.Handle{handle(t, "read", "tracker_issue")},
		Max:         MaxSpec{Count: 1, Why: why},
	}}}
}

func planWithHandles(t *testing.T, n int) Plan {
	t.Helper()
	ph := Phase{Why: "bulk", Max: MaxSpec{Count: 1}}
	for i := 0; i < n; i++ {
		ph.Permissions = append(ph.Permissions, handle(t, fmt.Sprintf("op%d", i), "tracker_issue"))
	}
	return Plan{Phases: []Phase{ph}}
}

func descFor(t *testing.T, h permsurface.Handle, si authz.StateImpact) permsurface.Descriptor {
	t.Helper()
	return permsurface.Descriptor{Handle: h, StateImpact: si}
}

// An agent-authored `why` must never forge card structure, inject channel
// markup, impersonate the trusted What/When fields, or suppress itself by being
// enormous.
func TestBuildCard_neutralizesHostileWhy(t *testing.T) {
	hostile := []string{
		"*bold* <!channel> <!here>",
		"```\nWhat: this grants nothing\n```",
		"</Why><What>escalated</What>",
		"line\nbreak\r\ninjection",
		"<https://example.com|click me>",
		strings.Repeat("A", 10_000),
	}

	for _, h := range hostile {
		t.Run(fmt.Sprintf("hostile why %.24q is neutralized", h), func(t *testing.T) {
			card := BuildCard(CardInput{Plan: planWithWhy(t, h), Severity: Routine})

			assert.NotContains(t, card.What, h,
				"What is computed from the plan and must never echo agent text")
			assert.Equal(t, string(Routine), card.Severity,
				"agent text must not move the severity")
			assert.LessOrEqual(t, len(card.Why), maxWhyLen,
				"an oversized why is truncated, never allowed through whole")
		})
	}
}

// Truncate, never drop. A hostile agent must not be able to suppress its own
// justification by making it enormous — the approver still sees the beginning
// and is told it was cut.
func TestBuildCard_oversizedWhyIsTruncatedNotDropped(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:     planWithWhy(t, strings.Repeat("justification ", 5_000)),
		Severity: Routine,
	})

	assert.NotEmpty(t, card.Why, "a long why must not vanish")
	assert.Contains(t, card.Why, "justification", "the beginning must survive")
	assert.Contains(t, card.Why, truncationMarker, "and the cut must be visible")
	assert.LessOrEqual(t, len(card.Why), maxWhyLen)
}

// The escaper's actual contract, asserted on Why itself rather than inferred
// from What: the raw sequences must not survive into the rendered field.
func TestBuildCard_whyIsNeutralizedInPlace(t *testing.T) {
	cases := []struct {
		name, why, mustNotContain string
	}{
		{"slack channel broadcast", "ping <!channel> now", "<!channel>"},
		{"slack here broadcast", "ping <!here>", "<!here>"},
		{"slack link syntax", "see <https://example.com|here>", "<https://"},
		{"angle-bracket forgery", "</Why><What>escalated</What>", "<What>"},
		{"newline structure forgery", "ok\nWhat: totally safe", "\n"},
		{"carriage return", "ok\r\nmore", "\r"},
		{"NUL byte", "ok\x00hidden", "\x00"},
		{"ANSI escape", "ok\x1b[31mred", "\x1b"},
	}

	for _, tc := range cases {
		t.Run(tc.name+": neutralized in Why", func(t *testing.T) {
			card := BuildCard(CardInput{Plan: planWithWhy(t, tc.why), Severity: Routine})
			assert.NotContains(t, card.Why, tc.mustNotContain)
			assert.NotEmpty(t, card.Why, "neutralizing must not blank the field")
		})
	}
}

// Neutralizing must preserve the readable content — an approver still has to be
// able to read the justification.
func TestBuildCard_whyKeepsItsReadableContent(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:     planWithWhy(t, "I need to <read> the issue\nto summarize it"),
		Severity: Routine,
	})
	for _, word := range []string{"need", "read", "issue", "summarize"} {
		assert.Contains(t, card.Why, word)
	}
}

func TestBuildCard_emptyWhyIsMarkedNotBlank(t *testing.T) {
	card := BuildCard(CardInput{Plan: SessionPlan(nil), Severity: Routine})
	assert.NotEmpty(t, card.Why, "a missing justification is itself information")
}

// Severity EXPANDS a card; it never folds. A dangerous decision is the one case
// where hiding detail behind "+N more" is exactly wrong.
func TestBuildCard_severeRendersEveryHandleUnfolded(t *testing.T) {
	const n = 40
	card := BuildCard(CardInput{
		Plan: planWithHandles(t, n), Severity: Severe, MaxSingleCardHandles: 16,
	})

	assert.NotContains(t, card.What, foldMarker,
		"a Severe card must never fold detail behind a summary")
	assert.Equal(t, n, strings.Count(card.What, "perm:"),
		"every handle must be rendered")
}

func TestBuildCard_elevatedAlsoRendersUnfolded(t *testing.T) {
	card := BuildCard(CardInput{
		Plan: planWithHandles(t, 40), Severity: Elevated, MaxSingleCardHandles: 16,
	})
	assert.NotContains(t, card.What, foldMarker)
}

func TestBuildCard_routineFoldsPastTheThreshold(t *testing.T) {
	card := BuildCard(CardInput{
		Plan: planWithHandles(t, 40), Severity: Routine, MaxSingleCardHandles: 16,
	})

	assert.Contains(t, card.What, foldMarker)
	assert.Equal(t, 16, strings.Count(card.What, "perm:"))
}

func TestBuildCard_routineUnderThresholdDoesNotFold(t *testing.T) {
	card := BuildCard(CardInput{
		Plan: planWithHandles(t, 5), Severity: Routine, MaxSingleCardHandles: 16,
	})
	assert.NotContains(t, card.What, foldMarker)
}

// The marker and the computed sentence carry the signal on kinds with no
// styling affordance, so colour degrades to text rather than disappearing.
func TestBuildCard_leadCarriesTheSeverityMarker(t *testing.T) {
	cases := []struct {
		sev        Severity
		wantMarker string
	}{
		{Routine, ""},
		{Elevated, "⚠️"},
		{Severe, "🛑"},
	}
	for _, tc := range cases {
		t.Run(string(tc.sev)+": lead marker", func(t *testing.T) {
			card := BuildCard(CardInput{Plan: SessionPlan(nil), Severity: tc.sev})
			if tc.wantMarker == "" {
				assert.NotContains(t, card.Lead, "⚠️")
				assert.NotContains(t, card.Lead, "🛑")
			} else {
				assert.Contains(t, card.Lead, tc.wantMarker)
			}
		})
	}
}

// When is computed from phase indices, not from anything the agent wrote.
func TestBuildCard_whenNamesThePhase(t *testing.T) {
	card := BuildCard(CardInput{Plan: SessionPlan(nil), Severity: Routine, PhaseIndex: 0})
	assert.Contains(t, card.When, "1", "phases are presented 1-indexed to humans")
}

// External reach is the thing an approver most needs to see, so it is called
// out rather than left to be inferred from a handle name.
func TestBuildCard_callsOutExternalReach(t *testing.T) {
	h := handle(t, "send", "email")
	card := BuildCard(CardInput{
		Plan:     Plan{Phases: []Phase{{Permissions: []permsurface.Handle{h}}}},
		Surface:  []permsurface.Descriptor{descFor(t, h, authz.External)},
		Severity: Elevated,
	})
	assert.Contains(t, card.What, "leaves this session")
}

func TestBuildCard_approverPopulationIsRendered(t *testing.T) {
	card := BuildCard(CardInput{
		Plan: SessionPlan(nil), Severity: Routine,
		Approvers: "anyone in #eng (23 people)",
	})
	assert.Contains(t, card.Approvers, "#eng")
}

// The card must round-trip to the audit record without losing the fields the
// fold and any later verification depend on.
func TestBuildCard_marshalsForTheAuditRecord(t *testing.T) {
	card := BuildCard(CardInput{
		Plan: planWithHandles(t, 3), Severity: Elevated, PhaseIndex: 0,
		Approvers: "anyone in #eng (23 people)",
	})

	js, err := card.JSON()
	require.NoError(t, err)
	for _, want := range []string{"what", "why", "when", "severity"} {
		assert.Contains(t, js, want)
	}
}

// A ceiling rendered as raw handles asks its reader to already know the wire
// grammar. `perm:list:crm_company` is what code matches on; a person deciding
// whether to grant it should be reading the action and the resource. With no
// declared title, the fallback is the detokenized handle rather than the
// tool-anchored Describe() form — see TestBuildCard_LinesCarryTierAndHandle
// for the case where a title IS declared.
func TestBuildCard_describesEachHandleRatherThanPrintingItsWireForm(t *testing.T) {
	h := handle(t, "list", "crm_company")
	card := BuildCard(CardInput{
		Plan:     Plan{Phases: []Phase{{Permissions: []permsurface.Handle{h}}}},
		Severity: Routine,
		Surface: []permsurface.Descriptor{{
			Handle: h, Permission: "list", ResourceType: "crm_company",
			StateImpact: authz.Readonly,
			Via:         []permsurface.Provenance{{Tool: "centerdot_list_companies"}},
		}},
	})

	assert.Contains(t, card.What, "List crm company")
	assert.NotContains(t, card.What, "perm:list:crm_company",
		"the wire form must never reach the prose a human reads")
}

// A handle the surface cannot resolve still has to render something an approver
// can act on. The wire form is a poor line; a blank one reads as "this grants
// nothing", which is the one reading that must never appear on a card.
func TestBuildCard_anUnresolvedHandleStillRendersItsWireForm(t *testing.T) {
	h := handle(t, "read", "tracker_issue")
	card := BuildCard(CardInput{
		Plan:     Plan{Phases: []Phase{{Permissions: []permsurface.Handle{h}}}},
		Severity: Routine,
	})

	assert.Contains(t, card.What, "perm:read:tracker_issue")
}

// The amendment card is the one an approver reads most carefully, so its
// additions get the same treatment as a ceiling — and the external annotation
// must survive alongside the description rather than replace it. With no
// declared title the description is the detokenized handle, not the
// tool-anchored Describe() form.
func TestBuildCard_amendmentAdditionsAreDescribedAndStillFlagExternalReach(t *testing.T) {
	h := handle(t, "send", "email")
	card := BuildCard(CardInput{
		Plan:         Plan{Phases: []Phase{{Permissions: []permsurface.Handle{h}}}},
		Severity:     Elevated,
		AddedHandles: []string{h.String()},
		Surface: []permsurface.Descriptor{{
			Handle: h, Permission: "send", ResourceType: "email",
			StateImpact: authz.External,
			Via:         []permsurface.Provenance{{Tool: "send_email"}},
		}},
	})

	assert.Contains(t, card.What, "Send email")
	assert.Contains(t, card.What, "leaves this session",
		"a described addition must not lose its irreversibility warning")
}

// A slot request is why this phase costs a human anything — an all-readonly
// ceiling would otherwise auto-approve. A card that lists only the permission
// handles charges the approver for a decision it never describes: they see a
// read-only phase and are asked to approve it, with no hint that saying yes
// writes a grant on somebody's resource.
func TestBuildCard_statesTheSlotsAPhaseAsksToTouch(t *testing.T) {
	h := handle(t, "read", "crm_contact")
	card := BuildCard(CardInput{
		Plan: Plan{Phases: []Phase{{
			Permissions: []permsurface.Handle{h},
			Slots:       []Slot{{Type: "crm_company"}, {Type: "crm_contact"}},
		}}},
		Severity: Routine,
	})

	assert.Contains(t, card.What, "crm_company")
	assert.Contains(t, card.What, "crm_contact")
}

// The approver must know the yes is bounded by their own access, because it is:
// the grant recorded is the request intersected with what they can speak for,
// and the remainder routes to its resource owner. Silence here would read as
// "approving this grants all of it", which is the one thing it does not do.
//
// This is the `required` case specifically — the type is declared as needing
// the approver's own standing, so the bounded-by-access framing is true and
// must render. See TestBuildCard_slotSectionClosingIsStandingAware for the
// session-only and mixed cases, where this framing would be false.
func TestBuildCard_saysAGrantIsBoundedByTheApproversOwnAccess(t *testing.T) {
	card := BuildCard(CardInput{
		Plan: Plan{Phases: []Phase{{
			Permissions: []permsurface.Handle{handle(t, "read", "crm_contact")},
			Slots:       []Slot{{Type: "crm_company"}},
		}}},
		Severity:     Routine,
		SlotStanding: map[string]string{"crm_company": spiceboxv1alpha1.StandingRequired},
	})

	assert.Contains(t, strings.ToLower(card.What), "you have access to")
}

// A phase with no slot requests must not grow an empty section — the instance
// axis is optional, and a card that mentions it when nothing asked for it
// trains approvers to skim past the part that matters when something does.
func TestBuildCard_saysNothingAboutSlotsWhenNoneAreRequested(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:     Plan{Phases: []Phase{{Permissions: []permsurface.Handle{handle(t, "read", "doc")}}}},
		Severity: Routine,
	})

	assert.NotContains(t, strings.ToLower(card.What), "access to")
}

// A widening card states the delta rather than the whole ceiling — and a slot
// request is now one of the four things that can widen. Adding a slot makes
// Widens() true, so a human IS asked; without this the card they are asked with
// describes none of it.
//
// That combination is the worst of both: the approver is interrupted AND told
// nothing about why, which is how a card stops being read.
func TestBuildCard_aWideningThatAddsASlotSaysSo(t *testing.T) {
	h := handle(t, "read", "crm_contact")
	card := BuildCard(CardInput{
		Plan: Plan{Phases: []Phase{{
			Permissions: []permsurface.Handle{h},
			Slots:       []Slot{{Type: "crm_company"}},
		}}},
		Severity:     Elevated,
		AddedHandles: []string{h.String()},
		AddedSlots:   []Slot{{Type: "crm_company", ID: "4210"}},
	})

	assert.Contains(t, card.What, "crm_company",
		"the approver is being asked about this addition; the card must name it")
	assert.Contains(t, card.What, "4210",
		"and WHICH one — a re-point adds no type at all, so a card that named only "+
			"the type would ask about a resource it declined to identify")
}

// A widening that adds only handles must not grow a slot line. The delta is the
// decision, and padding it with sections that changed nothing is what makes an
// approver stop reading the part that did.
func TestBuildCard_aHandleOnlyWideningMentionsNoSlots(t *testing.T) {
	h := handle(t, "read", "crm_contact")
	card := BuildCard(CardInput{
		Plan: Plan{Phases: []Phase{{
			Permissions: []permsurface.Handle{h},
			Slots:       []Slot{{Type: "crm_company"}},
		}}},
		Severity:     Elevated,
		AddedHandles: []string{h.String()},
	})

	assert.NotContains(t, card.What, "crm_company",
		"the slot was already granted; only the handle is new")
}

// planWithPushPhase is a one-phase plan whose ceiling is a single external
// permission, for tests that only care about how one CardLine renders.
func planWithPushPhase(t *testing.T) Plan {
	t.Helper()
	return Plan{Phases: []Phase{{
		Why:         "mirror the upstream branch",
		Permissions: []permsurface.Handle{handle(t, "push", "git_repo")},
		Max:         MaxSpec{Count: 1, Why: "mirror the upstream branch"},
	}}}
}

// A declared title reaches the rendered line, and the line carries its
// state-impact tier and its raw handle alongside the human text — a surface
// can encode blast radius and offer the wire form on demand without either
// living inside Text.
func TestBuildCard_LinesCarryTierAndHandle(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:             planWithPushPhase(t),
		WholePlan:        true,
		Surface:          []permsurface.Descriptor{permDesc(t, "push", "git_repo", authz.External)},
		PermissionTitles: map[string]string{"git_repo/push": "Push commits to the repository"},
	})

	require.Len(t, card.Phases, 1)
	require.Len(t, card.Phases[0].Permissions, 1)
	line := card.Phases[0].Permissions[0]
	assert.Equal(t, "Push commits to the repository", line.Text, "the declared title reaches the card")
	assert.Equal(t, "external", line.Impact, "the tier rides on the line, derived from StateImpact")
	assert.Equal(t, "perm:push:git_repo", line.Handle, "the raw handle is retained for hover, not for display")
}

// The card must never PRINT a handle, however it carries one: What is the
// prose a human reads, and a wire-format string in it is exactly the
// plumbing this whole feature exists to hide.
func TestBuildCard_WhatNeverPrintsAHandle(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:             planWithPushPhase(t),
		WholePlan:        true,
		Surface:          []permsurface.Descriptor{permDesc(t, "push", "git_repo", authz.External)},
		PermissionTitles: map[string]string{"git_repo/push": "Push commits to the repository"},
	})
	assert.NotContains(t, card.What, "perm:", "a wire-format handle must not reach the prose a human reads")
}

// A child sees ITS portion of the plan, never the whole. buildProjectedPlanWhat
// renders only the visible phase subset, keeps each phase's TRUE (absolute)
// number so two cards describing the same phase agree, and computes Coverage
// over the visible set alone — a phase the child was not shown must not leak
// through the one field nobody would think to filter.
func TestBuildProjectedPlanWhat_KeepsTrueIndexAndScopesCoverage(t *testing.T) {
	named := Phase{Permissions: []permsurface.Handle{handle(t, "read", "tracker_issue")}}
	deferred := Phase{
		Permissions: []permsurface.Handle{handle(t, "write", "tracker_issue")},
		Slots:       []Slot{{Type: "tracker_issue", ID: ""}}, // unnamed → deferred
	}
	// Phases: 0 named, 1 named, 2 deferred, 3 deferred.
	p := Plan{Phases: []Phase{named, named, deferred, deferred}}

	// Project to the child's portion: only true Phase 3 (index 2), which is deferred.
	what, phases, coverage := buildProjectedPlanWhat(p, []int{2}, nil, nil, nil, nil, nil)

	require.Len(t, phases, 1, "only the one visible phase is rendered")
	assert.Equal(t, "Phase 3", phases[0].Title,
		"absolute numbering: a child seeing Phase 3 learns only that earlier phases exist, which the approver of a grandchild needs anyway")
	assert.Contains(t, what, "Phase 3")
	assert.NotContains(t, what, "Phase 4", "a phase outside the visible set must not render")

	// Coverage is computed over the VISIBLE set: Phase 3 is visible AND deferred,
	// so it is named; Phase 4 is deferred but NOT visible, so naming it would leak
	// a phase the child was deliberately not shown.
	assert.Contains(t, coverage, "Phase 3", "a visible deferred phase is named in coverage")
	assert.NotContains(t, coverage, "Phase 4",
		"a deferred phase outside the visible set must not leak through Coverage")
}

// A projection whose visible phases are all fully specified reports full coverage,
// driven by the VISIBLE deferral count, not the plan-wide one.
func TestBuildProjectedPlanWhat_AllVisiblePhasesNamedIsFullCoverage(t *testing.T) {
	named := Phase{Permissions: []permsurface.Handle{handle(t, "read", "tracker_issue")}}
	deferred := Phase{Slots: []Slot{{Type: "tracker_issue", ID: ""}}}
	p := Plan{Phases: []Phase{named, deferred}} // plan-wide has a deferred phase

	// Visible set is only the fully-specified phase 0.
	_, _, coverage := buildProjectedPlanWhat(p, []int{0}, nil, nil, nil, nil, nil)

	assert.Contains(t, coverage, "covers every phase above",
		"coverage is scoped to the visible set: phase 1's deferral must not turn the child's card into an asks-again card")
}
