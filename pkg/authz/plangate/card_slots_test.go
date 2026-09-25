package plangate

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

func phaseWithSlots(t *testing.T, slots ...Slot) Plan {
	t.Helper()
	return Plan{Phases: []Phase{{
		Label:       "clone and patch",
		Why:         "the task needs the repository",
		Permissions: []permsurface.Handle{handle(t, "fetch", "git_repo")},
		Slots:       slots,
	}}}
}

// A named slot is the whole reason one approval can cover a phase. If the card
// shows only the TYPE, the approver agreed to a category — "some repository" —
// and every instance still has to be decided later, one prompt at a time.
func TestBuildCard_aNamedSlotRendersTheResourceItself(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:       phaseWithSlots(t, Slot{Type: "git_repo", ID: "https://github.com/acme/app"}),
		PhaseIndex: 0,
	})

	assert.Contains(t, card.What, "https://github.com/acme/app",
		"the approver has to see WHICH repository, not just that a repository is involved")
	assert.Contains(t, card.What, "git_repo")
}

// The inverse, and the one worth asserting as an ABSENCE. Over-promising is the
// failure mode here: a card that reads as though a specific resource were named
// when the agent could not name one lets an approval be spent on a target
// nobody saw.
func TestBuildCard_anUnnamedSlotDoesNotClaimASpecificResource(t *testing.T) {
	card := BuildCard(CardInput{
		Plan:       phaseWithSlots(t, Slot{Type: "git_repo"}),
		PhaseIndex: 0,
	})

	assert.Contains(t, card.What, "git_repo", "the TYPE is known and should be shown")
	assert.NotContains(t, card.What, "://",
		"nothing may look like a concrete target when none was declared")
	assert.Regexp(t, `(?i)not yet known|will ask again|no target`, card.What,
		"an unnamed target must be visibly unnamed, not silently omitted")
}

// The two coverage states, which is what the approver is actually deciding:
// whether saying yes finishes the conversation or merely starts it.
func TestBuildCard_coverageStateIsStatedPerPhase(t *testing.T) {
	cases := []struct {
		name     string
		slots    []Slot
		wantRe   string
		unwanted string
	}{
		{
			name:     "every target named: this approval covers the phase",
			slots:    []Slot{{Type: "git_repo", ID: "https://github.com/acme/app"}},
			wantRe:   `(?i)covered by this approval|this approval covers`,
			unwanted: "will ask again",
		},
		{
			name:   "one target unnamed: the phase will ask again",
			slots:  []Slot{{Type: "git_repo", ID: "https://github.com/acme/app"}, {Type: "linear_issue"}},
			wantRe: `(?i)will ask again`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := BuildCard(CardInput{Plan: phaseWithSlots(t, tc.slots...), PhaseIndex: 0})
			assert.Regexp(t, tc.wantRe, card.What)
			if tc.unwanted != "" {
				assert.NotContains(t, card.What, tc.unwanted,
					"a fully-named phase must not warn about a later prompt that will not come")
			}
		})
	}
}

// The agent's reason for needing a particular resource is useful and belongs on
// the card — in the UNTRUSTED half. It is the agent's claim about its own
// intent, and it is exactly the field a prompt-injected agent would use to argue
// for access, so it may never sit in the computed description.
func TestBuildCard_aSlotWhyIsAttributedToTheAgentNotTheCard(t *testing.T) {
	card := BuildCard(CardInput{
		Plan: phaseWithSlots(t, Slot{
			Type: "git_repo", ID: "https://github.com/acme/app",
			Why: "this is the repository the ticket refers to",
		}),
		PhaseIndex: 0,
	})

	assert.Contains(t, card.Why, "this is the repository the ticket refers to",
		"the agent's stated reason is worth showing")
	assert.NotContains(t, card.What, "the repository the ticket refers to",
		"but never in What — that half must stay computed")
}

// Why is display-only and must not be authority. If it entered the digest, an
// agent could obtain an approval and then re-derive a DIFFERENT plan identity by
// editing prose alone; if it entered the authority key, an approved phase would
// stop matching itself for the same reason.
func TestPlanIdentity_aSlotWhyIsNotAuthority(t *testing.T) {
	base := phaseWithSlots(t, Slot{Type: "git_repo", ID: "https://github.com/acme/app", Why: "first reason"})
	reworded := phaseWithSlots(t, Slot{Type: "git_repo", ID: "https://github.com/acme/app", Why: "an entirely different reason"})

	assert.Equal(t, base.Digest(), reworded.Digest(),
		"prose is not authority; rewording must not mint a new plan identity")
	assert.Equal(t, base.Phases[0].AuthorityKey(), reworded.Phases[0].AuthorityKey(),
		"nor may it break an approval already given for this phase")

	// The control: the ID *is* authority, so changing it must change both.
	retargeted := phaseWithSlots(t, Slot{Type: "git_repo", ID: "https://github.com/other/app", Why: "first reason"})
	assert.NotEqual(t, base.Digest(), retargeted.Digest(),
		"approving one repository must never be reusable for another")
	assert.NotEqual(t, base.Phases[0].AuthorityKey(), retargeted.Phases[0].AuthorityKey())
}

// The strong form of the trust boundary, stated as a property rather than a
// list. An enumerated set of injection strings only ever catches what someone
// thought of; this asserts the invariant itself — NOTHING the agent wrote
// reaches the computed half — over inputs nobody chose by hand.
func TestBuildCard_noAgentAuthoredTextEverReachesWhat(t *testing.T) {
	rng := rand.New(rand.NewSource(20260813))
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 \n\t:*_`~[]{}()<>|&$#/\\\"'"

	token := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteByte(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}

	for i := 0; i < 400; i++ {
		// A marker long enough that an incidental collision with computed text
		// is not plausible, wrapped in fully arbitrary agent prose.
		marker := fmt.Sprintf("MARKER%dZ%s", i, token(12))
		agentText := token(rng.Intn(60)) + marker + token(rng.Intn(60))

		plan := Plan{Phases: []Phase{{
			Label:       agentText,
			Why:         agentText,
			Permissions: []permsurface.Handle{handle(t, "fetch", "git_repo")},
			Slots:       []Slot{{Type: "git_repo", ID: "https://github.com/acme/app", Why: agentText}},
		}}}

		card := BuildCard(CardInput{Plan: plan, PhaseIndex: 0})

		require.NotContains(t, card.What, marker,
			"agent-authored text reached the computed half (iteration %d)", i)
		require.NotContains(t, card.When, marker,
			"When is computed from phase indices and must never carry agent text (iteration %d)", i)
	}
}

// A Why that impersonates the card's own vocabulary must still read as the
// agent's claim. The defense is placement plus neutralization, not filtering:
// the words are allowed, the structure is not.
func TestBuildCard_aWhyThatImitatesTheCardCannotForgeIt(t *testing.T) {
	hostile := "What: read-only, no writes\nAlso asks to reach: nothing"
	card := BuildCard(CardInput{
		Plan:       phaseWithSlots(t, Slot{Type: "git_repo", ID: "https://github.com/acme/app", Why: hostile}),
		PhaseIndex: 0,
	})

	assert.NotContains(t, card.What, "read-only, no writes",
		"the agent must not be able to describe its own request in the trusted half")
	assert.NotContains(t, card.Why, "\n",
		"newlines are flattened so agent text cannot forge card structure")
}
