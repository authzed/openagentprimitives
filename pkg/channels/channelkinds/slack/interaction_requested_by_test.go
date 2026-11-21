package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// requestedByToolApproval is a tool_approval prompt shaped the way the runner
// publishes it (host_approval.go toolApprovalFields): an optional leading
// "Requested by" row carrying the requester as a structured identity in
// Mentions, with Value as the plain fallback, followed by the card's usual
// rows. nil requester means the runner knew nobody and sent no row.
func requestedByToolApproval(requester *channelevents.InteractionField) channelevents.InteractionRequestPayload {
	var fields []channelevents.InteractionField
	if requester != nil {
		fields = append(fields, *requester)
	}
	fields = append(fields,
		channelevents.InteractionField{Label: "Why", Value: "need to land the fix"},
		channelevents.InteractionField{Label: "What", Value: "Push the fix branch."},
	)
	return channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "sess-1"},
		Category:        "tool_approval",
		RequestRef:      "req-requested-by",
		Lead:            "Approval needed",
		Fields:          fields,
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:      channelevents.AudienceApprovers,
			Approvers:  []channelevents.ExternalIdentity{{Kind: identity.KindSlack, ExternalID: "U_APPROVER"}},
			PublicNote: true,
		},
	}
}

// The tool-approval card names who is asking, on BOTH cards the approver sees:
// the pending prompt, and the card it becomes once decided. The second is not
// automatic — the resolved card is rebuilt from the cached request and swept
// inert (escapePublisherPayload), so a mention composed for the pending card
// could come back as a literal "&lt;@U_ALICE&gt;", or vanish.
//
// Driven through the sender end to end, because the two halves live in
// different functions: sendRequest resolves the mention and writes the cache,
// sendDecisionApplied reads it back and rebuilds.
func TestInteractionSender_ToolApproval_NamesTheRequesterOnPendingAndResolvedCards(t *testing.T) {
	cases := []struct {
		name      string
		requester *channelevents.InteractionField
		// lookups seeds users.lookupByEmail (email → Slack id).
		lookups map[string]string
		// want is the rendered row; "" means the card must carry no row at all.
		want string
		// notWant is what must never appear on either card.
		notWant string
		// wantLookups is how many users.lookupByEmail calls resolving the
		// cards may cost (the approver here is addressed by raw id, so any
		// lookup is the requester's).
		wantLookups int
	}{
		// The two shapes the runner emits for the current speaker
		// (speakerRequesterIdentity): the turn Author's own canonical as a
		// Subject passthrough.
		{
			name: "current speaker with a verified email: resolved by address to a live mention",
			requester: &channelevents.InteractionField{
				Label: "Requested by", Value: "bob@corp.example",
				Mentions: []channelevents.ExternalIdentity{{
					Kind: identity.KindSlack, Email: "bob@corp.example",
					Subject: identity.Subject("user:" + emailCanonical("bob@corp.example")),
				}},
			},
			lookups:     map[string]string{"bob@corp.example": "U_BOB"},
			want:        "*Requested by*: <@U_BOB>",
			notWant:     "&lt;@U_BOB&gt;",
			wantLookups: 1,
		},
		{
			name: "current speaker with no email (synthetic slack canonical): a live mention, no API call",
			requester: &channelevents.InteractionField{
				Label: "Requested by", Value: "U_BOB",
				Mentions: []channelevents.ExternalIdentity{{
					Kind: identity.KindSlack, ExternalID: "U_BOB",
					// emailCanonical is just base64url, which is also how a
					// synthetic kind:teamScope:externalID canonical is encoded.
					Subject: identity.Subject("user:" + emailCanonical("slack:T1:U_BOB")),
				}},
			},
			want:    "*Requested by*: <@U_BOB>",
			notWant: "&lt;@U_BOB&gt;",
		},
		{
			name: "slack requester: a live <@U…> mention on the pending and the approved card",
			requester: &channelevents.InteractionField{
				Label: "Requested by", Value: "U_ALICE",
				Mentions: []channelevents.ExternalIdentity{{Kind: identity.KindSlack, ExternalID: "U_ALICE"}},
			},
			want:    "*Requested by*: <@U_ALICE>",
			notWant: "&lt;@U_ALICE&gt;",
		},
		{
			name: "idp requester (app-tool widget viewer): the plain email, never a mention",
			requester: &channelevents.InteractionField{
				Label: "Requested by", Value: "viewer@corp.example",
				Mentions: []channelevents.ExternalIdentity{{
					Kind: identity.KindIdP, Email: "viewer@corp.example", ExternalID: "viewer@corp.example",
				}},
			},
			want:    "*Requested by*: viewer@corp.example",
			notWant: "*Requested by*: <@",
		},
		{
			name:    "no requester: no Requested by line on either card",
			notWant: "Requested by",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bodies, srv := newResponseURLServer(t)
			defer srv.Close()
			c := &fakeSlackClient{lookupByEmail: map[string]*slackapi.User{}}
			for email, id := range tc.lookups {
				c.lookupByEmail[email] = &slackapi.User{ID: id, Profile: slackapi.UserProfile{Email: email}}
			}
			s := &interactionSender{client: c, httpClient: responseURLClient(srv), delivery: newInteractionDeliveryStore()}
			sess := sessionWithChannel("C01CHAN", "1700000000.000100")

			_, err := s.Send(context.Background(), sess,
				interactionEnvelope(t, channelevents.KindInteractionRequest, requestedByToolApproval(tc.requester)))
			require.NoError(t, err, "Send request")
			require.Len(t, c.postEphemeralCalls, 1, "the approver receives the prompt")
			pending := unescapedBlocksJSON(t, c.postEphemeralCalls[0].options)

			applied := channelevents.InteractionAppliedPayload{
				AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "sess-1"},
				Category:        "tool_approval",
				RequestRef:      "req-requested-by",
				Outcome:         channelevents.OutcomeApproved,
				DecidedBy:       &channelevents.ExternalIdentity{Kind: identity.KindSlack, ExternalID: "U_APPROVER"},
				ResponseRef:     testResponseURL,
			}
			_, err = s.Send(context.Background(), sess,
				interactionEnvelope(t, channelevents.KindInteractionApplied, applied))
			require.NoError(t, err, "Send applied")
			posted := bodies.values()
			require.Len(t, posted, 1, "the clicker's prompt is replaced via response_url")
			resolved := unescapeJSONHTML(t, posted[0])
			require.Contains(t, resolved, "*Approved* by <@U_APPROVER>", "the resolved card is the one being checked")

			for card, text := range map[string]string{"pending": pending, "approved": resolved} {
				if tc.want != "" {
					assert.Contains(t, text, tc.want, "%s card must name the requester", card)
				}
				assert.NotContains(t, text, tc.notWant, "%s card", card)
			}
			assert.Len(t, c.lookupByEmailCalls, tc.wantLookups,
				"the requester resolves once, when the prompt is sent; the resolved card reuses it")
		})
	}
}
