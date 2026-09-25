package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestCapApproverFanout(t *testing.T) {
	mk := func(subs ...string) []channelevents.ExternalIdentity {
		out := make([]channelevents.ExternalIdentity, 0, len(subs))
		for _, s := range subs {
			out = append(out, channelevents.ExternalIdentity{Kind: "slack", Subject: identity.Subject(s)})
		}
		return out
	}
	cases := []struct {
		name  string
		in    []channelevents.ExternalIdentity
		limit int
		// want lists the expected surviving Subjects, in the exact order
		// capApproverFanout must return them.
		want []string
	}{
		{
			"under limit: unchanged, input order preserved",
			mk("user:a@corp.example", "user:b@corp.example"), 10,
			[]string{"user:a@corp.example", "user:b@corp.example"},
		},
		{
			"at limit: unchanged, input order preserved",
			mk("user:a@corp.example", "user:b@corp.example"), 2,
			[]string{"user:a@corp.example", "user:b@corp.example"},
		},
		{
			// Input is deliberately out of order (c, a, b) so this catches
			// both a length-only regression AND an unsorted-truncation
			// regression: the survivors must be the fanoutKey-sorted-first
			// N (a, b), not the first N of input order (c, a) or any other
			// non-deterministic subset.
			"over limit: truncated to the sorted-first N by fanoutKey",
			mk("user:c@corp.example", "user:a@corp.example", "user:b@corp.example"), 2,
			[]string{"user:a@corp.example", "user:b@corp.example"},
		},
		{
			"zero limit: default (10), unchanged",
			mk("user:a@corp.example"), 0,
			[]string{"user:a@corp.example"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := capApproverFanout(logr.Discard(), tc.in, tc.limit, "default/demo-session")
			gotSubjects := make([]string, len(got))
			for i, e := range got {
				gotSubjects[i] = e.Subject.String()
			}
			assert.Equal(t, tc.want, gotSubjects)
		})
	}
}

// approverPayload is a two-approver tool_approval request: the shape the
// owner-derived approver model publishes when a resource has more than one
// data owner. Each approver carries a raw channel-native id plus a verified
// email — the email wins the canonical derivation, so delivery goes through a
// live users.lookupByEmail, which is where a per-recipient miss comes from.
func approverPayload(emails ...string) channelevents.InteractionRequestPayload {
	approvers := make([]channelevents.ExternalIdentity, 0, len(emails))
	for _, e := range emails {
		approvers = append(approvers, channelevents.ExternalIdentity{
			Kind:       "slack",
			ExternalID: identity.RawExternalID("U_" + strings.ToUpper(strings.SplitN(e, "@", 2)[0])),
			Email:      identity.Email(e),
		})
	}
	return channelevents.InteractionRequestPayload{
		Category:   "tool_approval",
		RequestRef: "req-fanout",
		Lead:       "Approve running git_push?",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: approvers,
		},
	}
}

// TestInteractionSender_Request_ApproverResolveFailure_DoesNotAbortFanout pins
// the per-recipient contract of the approver fan-out: an approver this
// workspace cannot resolve (users.lookupByEmail → users_not_found, e.g. a
// verified email with no account here) is that ONE approver's problem. Aborting
// the loop instead strands every remaining approver — nobody is asked, the
// session parks at AwaitingDecision until it times out, and the outbound relay
// only logs (surfaceDeliveryFailure is gated on KindUserMessage), so nothing is
// posted in-thread either.
//
// Matches the two sibling per-recipient skips directly above the resolve call
// ("skip it rather than failing every other recipient in the fan-out").
func TestInteractionSender_Request_ApproverResolveFailure_DoesNotAbortFanout(t *testing.T) {
	const (
		absentEmail  = "gone@corp.example"    // no account in this workspace
		presentEmail = "present@corp.example" // has one
		presentID    = "U_PRESENT"
		chanID       = "C_FANOUT"
		threadTS     = "1700000000.000010"
	)
	c := fakeClientWithUser(presentEmail, presentID)
	s := &interactionSender{client: c, delivery: newInteractionDeliveryStore()}

	// The unresolvable approver is FIRST, so the abort would swallow the second.
	p := approverPayload(absentEmail, presentEmail)
	_, err := s.Send(context.Background(), sessionWithChannel(chanID, threadTS),
		interactionEnvelope(t, channelevents.KindInteractionRequest, p))
	require.NoError(t, err, "one unresolvable approver must not fail the whole send")

	require.Len(t, c.postEphemeralCalls, 1, "the resolvable approver must still be asked")
	assert.Equal(t, presentID, c.postEphemeralCalls[0].userID, "the prompt went to the approver who exists")
	assert.Equal(t, []string{absentEmail, presentEmail}, c.lookupByEmailCalls,
		"both approvers must be attempted, in fan-out order")
}

// TestInteractionSender_Request_AllApproversUnresolvable_ReportsUndeliverable
// is the other half: when NO approver resolves, the fan-out must fall through
// to reportUndeliverable so the thread learns nothing is waiting on it. The
// abort skipped that branch entirely, leaving the failure as one log line.
func TestInteractionSender_Request_AllApproversUnresolvable_ReportsUndeliverable(t *testing.T) {
	const (
		chanID   = "C_FANOUT_NONE"
		threadTS = "1700000000.000011"
	)
	c := &fakeSlackClient{} // no user resolves
	s := &interactionSender{client: c, delivery: newInteractionDeliveryStore()}

	p := approverPayload("a@corp.example", "b@corp.example")
	_, err := s.Send(context.Background(), sessionWithChannel(chanID, threadTS),
		interactionEnvelope(t, channelevents.KindInteractionRequest, p))
	require.NoError(t, err, "an undeliverable fan-out is surfaced, not returned as an error")

	assert.Empty(t, c.postEphemeralCalls, "nobody was reachable, so no prompt was delivered")
	require.Len(t, c.postMessageCalls, 1, "the undeliverable notice must be posted in-thread")
	assert.Contains(t, strings.ToLower(msgOptionText(t, c.postMessageCalls[0].channelID, c.postMessageCalls[0].options)),
		"couldn't reach anyone",
		"the thread must be told nothing is waiting on it")
}
