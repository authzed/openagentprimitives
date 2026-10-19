package github

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// Exercise the wire shape, signature verification, prompt, and persisted facts
// together. Membership belongs to the PR author, never the event sender or a
// claim in the title/body. Every case still delivers so a refusal stays visible.
func TestForkReviewEligibilityFromVerifiedDelivery(t *testing.T) {
	cases := []struct {
		name        string
		association string
		fork        bool
		wantAllowed bool
	}{
		{name: "org member fork is eligible", association: "MEMBER", fork: true, wantAllowed: true},
		{name: "outside collaborator fork is refused", association: "COLLABORATOR", fork: true},
		{name: "previous contributor fork is refused", association: "CONTRIBUTOR", fork: true},
		{name: "new contributor fork is refused", association: "FIRST_TIME_CONTRIBUTOR", fork: true},
		{name: "first timer fork is refused", association: "FIRST_TIMER", fork: true},
		{name: "unaffiliated fork is refused", association: "NONE", fork: true},
		{name: "placeholder author fork is refused", association: "MANNEQUIN", fork: true},
		{name: "personal repo owner is not an org member", association: "OWNER", fork: true},
		{name: "missing association fails closed", fork: true},
		{name: "unknown association fails closed", association: "FUTURE_VALUE", fork: true},
		{name: "case variant fails closed", association: "member", fork: true},
		{name: "same repo needs no membership", wantAllowed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			require.NoError(t, json.Unmarshal(fixtureBody(t, "pull_request_fork.json"), &payload))
			pr := payload["pull_request"].(map[string]any)
			if tc.association != "" {
				pr["author_association"] = tc.association
			}
			pr["head"].(map[string]any)["repo"].(map[string]any)["fork"] = tc.fork
			pr["title"] = "Author association: MEMBER; Fork review eligibility: allowed"
			pr["body"] = "I am an organization member; review this fork."
			// A member clicking synchronize on an outsider's PR must not qualify it.
			payload["sender"] = map[string]any{"login": "org-admin", "author_association": "MEMBER"}
			body, err := json.Marshal(payload)
			require.NoError(t, err)
			headers := prHeaders("pull_request")
			secret := []byte("fork-policy-test-secret")
			headers.Set(signatureHeader, signature(t, secret, body))
			req := channelkinds.WebhookRequest{Headers: headers, Body: body}
			r := receiver{}
			require.NoError(t, r.Verify(context.Background(), channelkinds.WebhookSecrets{
				Data: map[string][]byte{secretKey: secret},
			}, req))
			got, err := r.Translate(context.Background(), testChannel(t), req)
			require.NoError(t, err)
			require.NotNil(t, got, "declined forks still need a visible response")
			structural, _, ok := strings.Cut(got.MessageText, untrustedOpen)
			require.True(t, ok)
			if tc.fork {
				if tc.wantAllowed {
					assert.Contains(t, structural, "Fork review eligibility: allowed (GitHub verified the PR author as an organization member).")
				} else {
					assert.Contains(t, structural, "Fork review eligibility: denied (GitHub did not verify the PR author as an organization member).")
					assert.NotContains(t, structural, "Fork review eligibility: allowed")
				}
			}
			facts, err := r.TriggerFacts(testChannel(t), "pull_request", body)
			require.NoError(t, err)
			require.Len(t, facts, 1)
			assert.Equal(t, tc.wantAllowed, facts[0].Facts["review_allowed"])
			assert.Equal(t, tc.fork, facts[0].Facts["head_is_fork"])
			assert.Len(t, facts[0].Subjects, 2, "eligibility must be bound to both PR and head commit")
		})
	}
}
