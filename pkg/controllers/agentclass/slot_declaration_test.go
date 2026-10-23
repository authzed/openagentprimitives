package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func slotClass(slots ...spiceboxv1alpha1.AuthzSlot) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{}
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Slots: slots}
	return ac
}

// The enum values are enforced by the CRD, but the CRD cannot express a
// relationship BETWEEN two fields. These are the cross-field rules, and each one
// exists because the combination it rejects would otherwise read as a protection
// that is not actually in force.
func TestValidateSlotDeclarations(t *testing.T) {
	cases := []struct {
		name       string
		slot       spiceboxv1alpha1.AuthzSlot
		wantReason string
		wantMsg    []string
	}{
		{
			name: "no new fields: valid (the pre-existing shape stays legal)",
			slot: spiceboxv1alpha1.AuthzSlot{ResourceType: "tracker_issue", Permission: "write"},
		},
		{
			name: "channel_thread with an explicit trust policy: valid",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "http_target", Permission: "reachable",
				FillFrom: []string{"channel_thread", "ask"}, AutoGrantFrom: []string{"owner"},
			},
		},
		{
			// The dangerous shape. autoGrantFrom governs channel_thread seeding
			// and nothing else, so declaring it without that source is a policy
			// that never runs — and it is the kind of policy whose ABSENCE is
			// invisible, because the field is right there in the YAML.
			name: "autoGrantFrom without channel_thread: rejected as an inert policy",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "http_target", Permission: "reachable",
				FillFrom: []string{"ask"}, AutoGrantFrom: []string{"owner"},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"autoGrantFrom", "channel_thread", "http_target"},
		},
		{
			name: "autoGrantFrom with no fillFrom at all: rejected for the same reason",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "http_target", Permission: "reachable",
				AutoGrantFrom: []string{"participants"},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"autoGrantFrom", "channel_thread"},
		},
		{
			// The mirror of the autoGrantFrom rule, pointing the other way: not
			// a constraint that never runs, but a CAPABILITY that never binds.
			// An author reading this class would believe the agent starts with
			// that repo in hand.
			name: "defaults without the default source: rejected as a binding that never happens",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "github_repo", Permission: "read",
				Defaults: []string{"demo-org/demo-repo"}, FillFrom: []string{"ask"},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"defaults", "default", "github_repo"},
		},
		{
			name: "defaults with the default source named: valid",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "github_repo", Permission: "read",
				Defaults: []string{"demo-org/demo-repo"}, FillFrom: []string{"default", "ask"},
			},
		},
		{
			name: "defaults with no fillFrom at all: valid (fillFrom only ever narrows)",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "github_repo", Permission: "read",
				Defaults: []string{"demo-org/demo-repo"},
			},
		},
		{
			name: "extractionPrompt without query or extract: rejected as a prompt nothing reads",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "tracker_issue", Permission: "write",
				ExtractionPrompt: "look for ENG-123 style issue keys", FillFrom: []string{"default"},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"extractionPrompt", "query", "tracker_issue"},
		},
		{
			name: "extractionPrompt with the extract alias: valid",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "tracker_issue", Permission: "write",
				ExtractionPrompt: "look for ENG-123 style issue keys", FillFrom: []string{"extract"},
			},
		},
		{
			name: "unknown fillFrom value: rejected (defence in depth behind the CRD enum)",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "tracker_issue", Permission: "write",
				FillFrom: []string{"telepathy"},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"fillFrom", "telepathy"},
		},
		{
			name: "observed as the sole fillFrom: valid (registered, not yet wired to a binding path)",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "git_commit", Permission: "read",
				FillFrom: []string{"observed"},
			},
		},
		{
			// trigger is in the CRD's kubebuilder enum (AuthzSlot.FillFrom) and
			// must be in this reconciler-side duplicate too, or an object naming
			// it never becomes admissible even though the apiserver already
			// accepted it — exactly the shipped reviewbot demo's own slot shape.
			name: "trigger as the sole fillFrom: valid (the pipeline binds it at delivery time)",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "github_pull_request", Permission: "write_memory",
				FillFrom: []string{"trigger"},
			},
		},
		{
			// The bypass this branch's gate must refuse: channel_thread seeds a
			// slot grant in channelsd's backfill OUTSIDE the admissibleCandidates
			// filter, so a gated slot it admits would bind with its precondition
			// unevaluated.
			name: "requires[] with an explicit channel_thread fillFrom: rejected as a gate bypass",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "git_commit", Permission: "read",
				FillFrom: []string{"channel_thread"},
				Requires: []spiceboxv1alpha1.SlotPrecondition{{
					CEL:              "facts.observed.is_cross_repository == false",
					UndeterminedHint: "observe it first",
					RefusalMessage:   "the head branch is on a fork",
				}},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"git_commit", "channel_thread", "seeds a slot grant in channelsd without evaluating that precondition"},
		},
		{
			// The row that pins AllowsFill over a contains-check: an empty
			// fillFrom ADMITS channel_thread (absence narrows nothing), so a gated
			// slot with no fillFrom is the likelier real mistake and must be
			// refused too. A contains-check would let this through.
			name: "requires[] with no fillFrom at all: rejected (empty fillFrom still admits channel_thread)",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "git_commit", Permission: "read",
				Requires: []spiceboxv1alpha1.SlotPrecondition{{
					CEL:              "facts.observed.is_cross_repository == false",
					UndeterminedHint: "observe it first",
					RefusalMessage:   "the head branch is on a fork",
				}},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"git_commit", "channel_thread", "seeds a slot grant in channelsd without evaluating that precondition"},
		},
		{
			// trigger is the second fill source that writes a slot grant
			// directly at GrantSlots, outside admissibleCandidates — exactly
			// like channel_thread — so the same bypass rule must refuse it.
			name: "requires[] with fillFrom: [\"trigger\"]: rejected as a gate bypass",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "github_pull_request", Permission: "write_memory",
				FillFrom: []string{"trigger"},
				Requires: []spiceboxv1alpha1.SlotPrecondition{{
					CEL:              "facts.observed.is_cross_repository == false",
					UndeterminedHint: "observe it first",
					RefusalMessage:   "the head branch is on a fork",
				}},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"github_pull_request", "trigger", "without evaluating that precondition"},
		},
		{
			// The mixed shape: trigger sits alongside a fill source that DOES
			// run the gate. A single-element contains-check on "trigger" alone
			// would still catch this; the point is that the predicate finds
			// trigger anywhere in the list, not only when it is the sole entry.
			name: "requires[] with fillFrom: [\"trigger\",\"observed\"]: rejected as a gate bypass",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "github_pull_request", Permission: "write_memory",
				FillFrom: []string{"trigger", "observed"},
				Requires: []spiceboxv1alpha1.SlotPrecondition{{
					CEL:              "facts.observed.is_cross_repository == false",
					UndeterminedHint: "observe it first",
					RefusalMessage:   "the head branch is on a fork",
				}},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"github_pull_request", "trigger", "without evaluating that precondition"},
		},
		{
			// The intended shape still admits: observed runs the gate, so a gated
			// slot restricted to it is exactly what the refusal steers authors to.
			name: "requires[] with a gate-running observed fillFrom: valid",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "git_commit", Permission: "read",
				FillFrom: []string{"observed"},
				Requires: []spiceboxv1alpha1.SlotPrecondition{{
					CEL:              "facts.observed.is_cross_repository == false",
					UndeterminedHint: "observe it first",
					RefusalMessage:   "the head branch is on a fork",
				}},
			},
		},
		{
			name: "unknown autoGrantFrom value: rejected",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "http_target", Permission: "reachable",
				FillFrom: []string{"channel_thread"}, AutoGrantFrom: []string{"everyone"},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"autoGrantFrom", "everyone"},
		},
		{
			name: "unknown membership value: rejected",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "tracker_issue", Permission: "write", Membership: "occasional",
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"membership", "occasional"},
		},
		{
			name: "membership frozen: valid",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "tracker_issue", Permission: "write", Membership: "frozen",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, reason, msg := validateSlotDeclarations(slotClass(tc.slot))
			if tc.wantReason == "" {
				assert.Empty(t, reason, "unexpected rejection: %s", msg)
				return
			}
			assert.Equal(t, tc.wantReason, reason)
			for _, want := range tc.wantMsg {
				assert.Contains(t, msg, want, "the message must be actionable without reading a spec")
			}
		})
	}
}

// Two entries for one resourceType make "which declaration governs?" ambiguous
// the moment they disagree — and they will disagree, because the whole point of
// the new fields is that two slots of the same type can want different trust.
func TestValidateSlotDeclarations_rejectsDuplicateResourceType(t *testing.T) {
	_, reason, msg := validateSlotDeclarations(slotClass(
		spiceboxv1alpha1.AuthzSlot{ResourceType: "dup_type", Permission: "read"},
		spiceboxv1alpha1.AuthzSlot{ResourceType: "dup_type", Permission: "write"},
	))
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, reason)
	assert.Contains(t, msg, "dup_type")
}

// A nil authz block, or one with no slots, must not trip anything — most classes
// declare no slots at all.
func TestValidateSlotDeclarations_emptyIsValid(t *testing.T) {
	_, reason, _ := validateSlotDeclarations(&spiceboxv1alpha1.AgentClass{})
	assert.Empty(t, reason, "a class with no authz block declares no slots")

	_, reason, _ = validateSlotDeclarations(slotClass())
	assert.Empty(t, reason, "an authz block with no slots is valid")
}

// Every slot is checked, not just the first — otherwise a bad declaration hides
// behind a good one and the class admits with a policy that never runs.
func TestValidateSlotDeclarations_checksEverySlot(t *testing.T) {
	_, reason, msg := validateSlotDeclarations(slotClass(
		spiceboxv1alpha1.AuthzSlot{ResourceType: "fine_type", Permission: "read"},
		spiceboxv1alpha1.AuthzSlot{
			ResourceType: "bad_type", Permission: "read",
			FillFrom: []string{"ask"}, AutoGrantFrom: []string{"owner"},
		},
	))
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, reason)
	assert.Contains(t, msg, "bad_type", "the message must name WHICH slot is wrong")
}

// requiring is a one-precondition slot whose message fields are filled in, so a
// case that means to exercise the CEL rules is never failed by an empty hint.
//
// It carries a gate-running fillFrom (observed): a gated slot with an empty
// fillFrom is now refused for admitting channel_thread (the channel_thread
// bypass rule), and that refusal — which runs before the preconditions are
// compiled — would otherwise preempt every CEL-compile case below.
func requiring(resourceType, expr string) spiceboxv1alpha1.AuthzSlot {
	return spiceboxv1alpha1.AuthzSlot{
		ResourceType: resourceType, Permission: "read",
		FillFrom: []string{"observed"},
		Requires: []spiceboxv1alpha1.SlotPrecondition{{
			CEL:              expr,
			UndeterminedHint: "call gitlike_gh pr view <n> --json isCrossRepository first",
			RefusalMessage:   "this pull request's head branch is on a fork",
		}},
	}
}

// A precondition is compiled at ADMISSION so its author learns it is broken
// here, where a message can name the offending entry — not at dispatch, where a
// predicate that cannot be evaluated surfaces as a slot that never binds for no
// visible reason.
//
// Each rejection below is the compiler's, not a rule restated here: this test
// pins that the reconciler actually runs precondition.Compile over every entry
// and reports what it says. The compiler's own reasoning has its own tests.
func TestValidateSlotDeclarations_preconditions(t *testing.T) {
	cases := []struct {
		name       string
		slot       spiceboxv1alpha1.AuthzSlot
		wantReason string
		wantMsg    []string
	}{
		{
			name: "well-formed predicate: valid",
			slot: requiring("git_commit", "facts.observed.is_cross_repository == false"),
		},
		{
			name:       "uncompilable CEL: rejected",
			slot:       requiring("git_commit", "facts.observed.is_cross_repository =="),
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"authz.slots[git_commit].requires[0]", "compile "},
		},
		{
			// The mistake an author makes first: a bare fact reads as dyn, and a
			// precondition that might not be a bool might not be a decision.
			name:       "non-bool CEL: rejected",
			slot:       requiring("git_commit", "facts.observed.is_cross_repository"),
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"authz.slots[git_commit].requires[0]", "must return bool"},
		},
		{
			// A typo'd namespace must not read as a fact that simply never
			// arrives, which would hold the slot undetermined forever.
			name:       "unknown provenance: rejected",
			slot:       requiring("git_commit", `facts.rumour.is_cross_repository == false`),
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"authz.slots[git_commit].requires[0]", "unknown fact provenance", "rumour"},
		},
		{
			// has() over a fact NAME turns "not yet observed" into a decidable
			// boolean, which collapses the tri-state the gate depends on.
			name:       "has() over a fact name: rejected",
			slot:       requiring("git_commit", "has(facts.observed.is_cross_repository)"),
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"authz.slots[git_commit].requires[0]", "has()"},
		},
		{
			// The three message fields carry MinLength=1 in the CRD. This is the
			// same defence in depth the enum maps above document: an object that
			// reached the reconciler through a client that skipped validation
			// must not be admitted with a gate that can only deny silently.
			name: "empty undeterminedHint: rejected",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "git_commit", Permission: "read",
				// observed so the channel_thread bypass rule does not preempt the
				// message-field check this row is about.
				FillFrom: []string{"observed"},
				Requires: []spiceboxv1alpha1.SlotPrecondition{{
					CEL: "facts.observed.ok == true", RefusalMessage: "no",
				}},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"authz.slots[git_commit].requires[0]", "undeterminedHint"},
		},
		{
			name: "empty refusalMessage: rejected",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "git_commit", Permission: "read",
				// observed so the channel_thread bypass rule does not preempt the
				// message-field check this row is about.
				FillFrom: []string{"observed"},
				Requires: []spiceboxv1alpha1.SlotPrecondition{{
					CEL: "facts.observed.ok == true", UndeterminedHint: "observe it first",
				}},
			},
			wantReason: spiceboxv1alpha1.ReasonSlotDeclarationInvalid,
			wantMsg:    []string{"authz.slots[git_commit].requires[0]", "refusalMessage"},
		},
		{
			name: "no requires at all: valid (preconditions are opt-in)",
			slot: spiceboxv1alpha1.AuthzSlot{ResourceType: "git_commit", Permission: "read"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, reason, msg := validateSlotDeclarations(slotClass(tc.slot))
			if tc.wantReason == "" {
				assert.Empty(t, reason, "unexpected rejection: %s", msg)
				return
			}
			assert.Equal(t, tc.wantReason, reason)
			for _, want := range tc.wantMsg {
				assert.Contains(t, msg, want, "the message must name the offending entry and say what is wrong")
			}
		})
	}
}

// Every entry is compiled, not just the first — otherwise a broken predicate
// hides behind a good one and the class admits carrying a gate that cannot run.
func TestValidateSlotDeclarations_checksEveryPrecondition(t *testing.T) {
	slot := requiring("git_commit", "facts.observed.is_cross_repository == false")
	slot.Requires = append(slot.Requires, spiceboxv1alpha1.SlotPrecondition{
		CEL:              "has(facts.envelope.head_is_fork)",
		UndeterminedHint: "observe the head first",
		RefusalMessage:   "the head is on a fork",
	})

	_, reason, msg := validateSlotDeclarations(slotClass(slot))
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, reason)
	assert.Contains(t, msg, "requires[1]", "the message must name WHICH precondition is wrong")
}

func TestValidateSlotDeclarations_SingleOccupancyRejectsMultipleDefaults(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{}
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Slots: []spiceboxv1alpha1.AuthzSlot{{
		ResourceType: "git_repo", Description: "d", Permission: "push",
		Occupancy: "single",
		Defaults:  []string{"github.com/a/one", "github.com/a/two"},
	}}}
	_, reason, msg := validateSlotDeclarations(ac)
	require.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, reason)
	assert.Contains(t, msg, "git_repo")
	assert.Contains(t, msg, "occupancy: multi")
}

func TestValidateSlotDeclarations_SingleOccupancyAllowsOneDefault(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{}
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Slots: []spiceboxv1alpha1.AuthzSlot{{
		ResourceType: "git_repo", Description: "d", Permission: "push",
		Occupancy: "single", Defaults: []string{"github.com/a/one"},
	}}}
	_, reason, _ := validateSlotDeclarations(ac)
	assert.Empty(t, reason)
}

// An omitted occupancy MEANS single (AuthzSlotOccupancyDefault) — this is the
// binding constraint, not a convenience: a slot written before the field
// existed must not silently become multi-occupancy.
func TestValidateSlotDeclarations_OmittedOccupancyDefaultsToSingle(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{}
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Slots: []spiceboxv1alpha1.AuthzSlot{{
		ResourceType: "git_repo", Description: "d", Permission: "push",
		Defaults: []string{"github.com/a/one", "github.com/a/two"},
	}}}
	_, reason, msg := validateSlotDeclarations(ac)
	require.Equal(t, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, reason)
	assert.Contains(t, msg, "git_repo")
	assert.Contains(t, msg, "occupancy: multi")
}

func TestValidateSlotDeclarations_MultiOccupancyAllowsSeveralDefaults(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{}
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Slots: []spiceboxv1alpha1.AuthzSlot{{
		ResourceType: "git_repo", Description: "d", Permission: "push",
		Occupancy: "multi",
		Defaults:  []string{"github.com/a/one", "github.com/a/two", "github.com/a/three"},
	}}}
	_, reason, msg := validateSlotDeclarations(ac)
	assert.Empty(t, reason, "unexpected rejection: %s", msg)
}
