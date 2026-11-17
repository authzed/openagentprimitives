package goals

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

func TestGroupExecutionTermsValidation(t *testing.T) {
	const owner = "group:g1#member"
	now := time.Now().UTC()
	valid := ExecutionTerms{
		ClassDigest:       "pin",
		DueAt:             now.Add(time.Minute),
		ExpiresAt:         now.Add(90 * 24 * time.Hour),
		Bounds:            ExecutionBounds{180, 10, 10000, 90},
		Group:             &GroupDestination{Channel: "team", ChannelUID: "uid", MemberGroup: owner},
		AllowedOperations: []string{"respond_to_user"},
		Evidence:          []string{"receipt"},
		ActionApproval:    "standing_plan",
		StandingPlan:      &plangate.Plan{Phases: []plangate.Phase{{Slots: []plangate.Slot{{Type: "repo", ID: "r1"}}}}},
	}
	require.NoError(t, valid.validate(now, owner))
	for _, tc := range []struct {
		name, contains string
		owner          string
		change         func(*ExecutionTerms)
	}{
		{"expiry 90d+1s: refused", "within 90 days", owner, func(x *ExecutionTerms) { x.ExpiresAt = now.Add(90*24*time.Hour + time.Second) }},
		{"standing_plan without plan: refused", "standing plan", owner, func(x *ExecutionTerms) { x.StandingPlan = nil }},
		{"empty-ID slot: refused naming the slot type", "repo", owner, func(x *ExecutionTerms) {
			x.StandingPlan = &plangate.Plan{Phases: []plangate.Phase{{Slots: []plangate.Slot{{Type: "repo"}}}}}
		}},
		{"standing_private on group goal: refused", "standing_private", owner, func(x *ExecutionTerms) { x.ActionApproval = "standing_private" }},
		{"group and destination together: refused", "exactly one", owner, func(x *ExecutionTerms) {
			x.Destination = PrivateDestination{Channel: "c", ChannelUID: "u", Recipient: owner}
		}},
		{"member group differs from owner: refused", "memberGroup", owner, func(x *ExecutionTerms) { x.Group.MemberGroup = "group:other#member" }},
		{"group terms on private goal: refused", "exactly one", "owner", func(*ExecutionTerms) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			terms := valid
			g := *valid.Group
			terms.Group = &g
			tc.change(&terms)
			err := terms.validate(now, tc.owner)
			require.ErrorIs(t, err, ErrInvalid)
			require.ErrorContains(t, err, tc.contains)
		})
	}
	t.Run("group goal without group destination: refused", func(t *testing.T) {
		terms := valid
		terms.Group = nil
		require.ErrorContains(t, terms.validate(now, owner), "exactly one")
	})
}

func TestPrivateExecutionTermsKeep30DayCap(t *testing.T) {
	now := time.Now().UTC()
	terms := ExecutionTerms{ClassDigest: "pin", DueAt: now.Add(time.Minute), ExpiresAt: now.Add(31 * 24 * time.Hour), Bounds: ExecutionBounds{180, 10, 10000, 90}, Destination: PrivateDestination{Channel: "p", ChannelUID: "u", Recipient: "owner"}, AllowedOperations: []string{"respond_to_user"}, Evidence: []string{"r"}}
	require.ErrorContains(t, terms.validate(now, "owner"), "within 30 days")
	terms.ActionApproval = "standing_plan"
	require.ErrorContains(t, terms.validate(now, "owner"), "group")
}
