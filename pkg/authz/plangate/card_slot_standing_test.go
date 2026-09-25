package plangate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// humanVerb is a CLOSED map: an unmapped permission must degrade to its own
// name — honest — never to an invented English word, because the string
// reaches a human on an approval card.
func TestHumanVerb(t *testing.T) {
	cases := []struct {
		name       string
		permission string
		want       string
	}{
		{"push maps to push", "push", "push"},
		{"read maps to read", "read", "read"},
		{"write maps to update", "write", "update"},
		{"fetch maps to fetch", "fetch", "fetch"},
		{"contact_access maps to contact", "contact_access", "contact"},
		{"unmapped permission falls back to its own name", "list_widgets", "list_widgets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, humanVerb(tc.permission))
		})
	}
}

// Vouching and delegating are different acts and must not read identically.
func TestSlotSentence(t *testing.T) {
	cases := []struct {
		name       string
		standing   string
		permission string
		want       string
	}{
		{
			name:       "session-only: the session gains the access for its lifetime",
			standing:   spiceboxv1alpha1.StandingSessionOnly,
			permission: "push",
			want:       "This session will be able to push for as long as it runs.",
		},
		{
			name:       "required: the approver is lending access they hold",
			standing:   spiceboxv1alpha1.StandingRequired,
			permission: "push",
			want:       "You already have this access; approving lends it to this session.",
		},
		{
			name:       "absent standing defaults to session-only, matching the runtime bypass",
			standing:   "",
			permission: "contact_access",
			want:       "This session will be able to contact for as long as it runs.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slotSentence(tc.standing, tc.permission)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The sentence is user-facing: no relation names, object ids or CRD kinds —
// slotSentence never takes a resource type or an instance id, so it cannot
// leak either by construction; this pins that as a property rather than
// trusting the signature.
func TestSlotSentence_LeaksNoInternalVocabulary(t *testing.T) {
	for _, standing := range []string{spiceboxv1alpha1.StandingSessionOnly, spiceboxv1alpha1.StandingRequired} {
		got := slotSentence(standing, "push")
		for _, leak := range []string{"slot_grant", "git_repo", "SpiceDB", "agentsession", "=3A", "perm:"} {
			assert.NotContains(t, got, leak, "internal vocabulary must not reach a chat surface")
		}
	}
}

// The trailing sentence states what saying yes actually buys, and it must be
// honest for every mix of standings a section can hold.
func TestSlotStandingSummary(t *testing.T) {
	required := map[string]string{"crm_company": spiceboxv1alpha1.StandingRequired}
	sessionOnly := map[string]string{"git_repo": spiceboxv1alpha1.StandingSessionOnly}
	mixed := map[string]string{
		"crm_company": spiceboxv1alpha1.StandingRequired,
		"git_repo":    spiceboxv1alpha1.StandingSessionOnly,
	}

	cases := []struct {
		name     string
		slots    []Slot
		standing map[string]string
		want     string
	}{
		{
			name:     "every slot required: unchanged sentence, still bounded by access",
			slots:    []Slot{{Type: "crm_company", ID: "4210"}},
			standing: required,
			want:     "Approving grants only the ones you have access to; the rest go to their owners.",
		},
		{
			name:     "every slot session-only: approving IS the authority, not the approver's access",
			slots:    []Slot{{Type: "git_repo", ID: "https://forge.example/acme/app"}},
			standing: sessionOnly,
			want:     "Approving grants this session access to the named resources for as long as it runs.",
		},
		{
			name:     "mixed: neither sentence alone is honest",
			slots:    []Slot{{Type: "crm_company", ID: "4210"}, {Type: "git_repo", ID: "https://forge.example/acme/app"}},
			standing: mixed,
			want: "Approving lends this session the ones you already have access to, " +
				"and grants the rest outright — both for as long as it runs.",
		},
		{
			name:     "an absent type in the map defaults to session-only, per the runtime bypass",
			slots:    []Slot{{Type: "git_repo", ID: "https://forge.example/acme/app"}},
			standing: nil,
			want:     "Approving grants this session access to the named resources for as long as it runs.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slotStandingSummary(tc.slots, tc.standing)
			assert.Equal(t, tc.want, got)
		})
	}
}

// A session-only phase's slot section must say so honestly at the CARD level,
// not just in the unit-tested helper — the whole point is what a human reads.
func TestBuildCard_slotSectionClosingIsStandingAware(t *testing.T) {
	base := func(standing map[string]string) Card {
		return BuildCard(CardInput{
			Plan: Plan{Phases: []Phase{{
				Permissions: []permsurface.Handle{handle(t, "fetch", "git_repo")},
				Slots:       []Slot{{Type: "git_repo", ID: "https://forge.example/acme/app"}},
			}}},
			Severity:     Routine,
			SlotStanding: standing,
		})
	}

	t.Run("required: keeps the bounded-by-access sentence", func(t *testing.T) {
		card := base(map[string]string{"git_repo": spiceboxv1alpha1.StandingRequired})
		assert.Contains(t, strings.ToLower(card.What), "you have access to")
	})

	t.Run("session-only: does not claim the grant is bounded by the approver's access", func(t *testing.T) {
		card := base(map[string]string{"git_repo": spiceboxv1alpha1.StandingSessionOnly})
		assert.NotContains(t, strings.ToLower(card.What), "you have access to",
			"a session-only grant does not depend on the approver's own access at all")
		assert.Contains(t, strings.ToLower(card.What), "for as long as it runs")
	})
}

// The per-slot line itself must render the standing-aware sentence, and the
// rendered card — not just the isolated helper — must leak none of the
// internal vocabulary a human should never see, for both standing values.
func TestBuildCard_perSlotSentenceLeaksNoInternalVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name         string
		standing     string
		wantSentence string
	}{
		{
			name:         "session-only",
			standing:     spiceboxv1alpha1.StandingSessionOnly,
			wantSentence: "This session will be able to push for as long as it runs.",
		},
		{
			name:         "required",
			standing:     spiceboxv1alpha1.StandingRequired,
			wantSentence: "You already have this access; approving lends it to this session.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The declared value (not the object id — see buildSlotSection's own
			// comment) is legitimately rendered verbatim in the "type — detail"
			// line, so it must contain none of the forbidden substrings itself, or
			// this test could not tell "leaked by the new sentence" apart from
			// "present in the fixture's own declared value". A resolved Surface
			// descriptor is supplied so the ceiling line describes the handle
			// ("Push git_repo") instead of falling back to its wire form
			// ("perm:push:git_repo") — a DIFFERENT, pre-existing leak this test is
			// not about; see TestBuildCard_anUnresolvedHandleStillRendersItsWireForm.
			h := handle(t, "push", "git_repo")
			card := BuildCard(CardInput{
				Plan: Plan{Phases: []Phase{{
					Permissions: []permsurface.Handle{h},
					Slots:       []Slot{{Type: "git_repo", ID: "acme/app"}},
				}}},
				Severity: Routine,
				Surface: []permsurface.Descriptor{{
					Handle: h, Permission: "push", ResourceType: "git_repo",
				}},
				SlotStanding:    map[string]string{"git_repo": tc.standing},
				SlotPermissions: map[string]string{"git_repo": "push"},
			})

			assert.Contains(t, card.What, tc.wantSentence)
			for _, leak := range []string{"slot_grant", "SpiceDB", "agentsession", "=3A", "perm:"} {
				assert.NotContains(t, card.What, leak, "internal vocabulary must not reach a chat surface")
			}
		})
	}
}
