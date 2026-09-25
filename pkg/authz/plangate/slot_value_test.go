package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A slot could name a TYPE but not an instance, so a plan could say "I need a
// github_repo slot" and never "I will read foo/bar". Pre-approval was therefore
// impossible: the approver could not see which resource they were granting, so
// every instance had to be decided later, one prompt at a time.
//
// Naming the value up front is what lets one approval cover the work.
func TestFreeze_carriesTheSlotValue(t *testing.T) {
	surface := demoSurface(t)
	plan, probs := FreezeFrom([]AuthoredPhase{{
		ID: "read", Label: "Read", Why: "the user named this repo",
		Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "to read"}},
		Slots:       []AuthoredSlot{{Type: "tracker_issue", ID: "foo/bar", Why: "the repo the user named"}},
	}}, surface, []string{"tracker_issue"})
	require.Empty(t, probs)
	require.Len(t, plan.Phases[0].Slots, 1)

	assert.Equal(t, "tracker_issue", plan.Phases[0].Slots[0].Type)
	assert.Equal(t, "foo/bar", plan.Phases[0].Slots[0].ID,
		"the value is what makes the approval specific; without it the approver "+
			"is agreeing to a category")
}

// THE reuse guard. A slot value is authority, so approving "read foo/bar" must
// not be reusable for "read baz/qux". If the value were outside the digest, an
// agent could have a plan approved and then swap the target while keeping the
// approval — exactly the reshaping the digest binding exists to prevent.
func TestDigest_slotValueIsAuthority(t *testing.T) {
	surface := demoSurface(t)
	mk := func(id string) Plan {
		p, probs := FreezeFrom([]AuthoredPhase{{
			ID: "read", Label: "Read", Why: "w",
			Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "w"}},
			Slots:       []AuthoredSlot{{Type: "tracker_issue", ID: id, Why: "w"}},
		}}, surface, []string{"tracker_issue"})
		require.Empty(t, probs)
		return p
	}
	assert.NotEqual(t, mk("foo/bar").Digest(), mk("baz/qux").Digest(),
		"two different resources must be two different plans")
	assert.Equal(t, mk("foo/bar").Digest(), mk("foo/bar").Digest(),
		"and the same one must be stable, or every re-plan re-charges the user")
}

// The agent's WHY is display-only and excluded from the digest — a value is
// authority, commentary is not. Changing the story must not invalidate an
// approval; changing the target must.
func TestDigest_slotWhyIsNotAuthority(t *testing.T) {
	surface := demoSurface(t)
	mk := func(why string) Plan {
		p, probs := FreezeFrom([]AuthoredPhase{{
			ID: "read", Label: "Read", Why: "w",
			Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "w"}},
			Slots:       []AuthoredSlot{{Type: "tracker_issue", ID: "foo/bar", Why: why}},
		}}, surface, []string{"tracker_issue"})
		require.Empty(t, probs)
		return p
	}
	assert.Equal(t, mk("first reason").Digest(), mk("a different reason").Digest())
}

// A slot with no value stays legal: it is the honest "I cannot name this yet"
// case, and the card marks such a phase as needing a later approval. Refusing it
// would force the agent to invent a target.
func TestFreeze_slotWithoutAValueIsStillLegal(t *testing.T) {
	surface := demoSurface(t)
	plan, probs := FreezeFrom([]AuthoredPhase{{
		ID: "act", Label: "Act", Why: "w",
		Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "w"}},
		Slots:       []AuthoredSlot{{Type: "tracker_issue", Why: "I will know once I look"}},
	}}, surface, []string{"tracker_issue"})
	require.Empty(t, probs)
	require.Len(t, plan.Phases[0].Slots, 1)
	assert.Empty(t, plan.Phases[0].Slots[0].ID)
}

// A valued slot and a value-less one of the same type are different authority
// and must not collide — otherwise "read foo/bar" and "read something, TBD"
// would share an approval.
func TestDigest_valuedAndUnvaluedSlotsDiffer(t *testing.T) {
	surface := demoSurface(t)
	mk := func(id string) Plan {
		p, _ := FreezeFrom([]AuthoredPhase{{
			ID: "read", Label: "Read", Why: "w",
			Permissions: []AuthoredPermission{{Handle: "perm:read:tracker_issue", Why: "w"}},
			Slots:       []AuthoredSlot{{Type: "tracker_issue", ID: id, Why: "w"}},
		}}, surface, []string{"tracker_issue"})
		return p
	}
	assert.NotEqual(t, mk("").Digest(), mk("foo/bar").Digest())
}
