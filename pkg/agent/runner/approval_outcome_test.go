package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
)

// TestApprovalFailureContent pins the unambiguous-outcome contract: a
// timed-out approval and an explicit deny must produce visibly different
// tool-result content. The agent saw a generic "approval timed out…"
// message in production and confabulated "Alice denied the request"
// to the user — exactly because both outcomes used similar phrasing.
// The fix is to make the message text load-bearing: timeouts shout
// "TIMEOUT, not a denial," and denies name the approver.
func TestApprovalFailureContent(t *testing.T) {
	cases := []struct {
		name           string
		toolName       string
		decision       approval.Decision
		awaitErr       error
		mustContain    []string
		mustNotContain []string
	}{
		{
			name:     "timeout: awaitErr set (context cancelled / deadline)",
			toolName: "hubspot_search_crm_objects",
			decision: approval.Decision{Reason: "timeout"},
			awaitErr: context.DeadlineExceeded,
			mustContain: []string{
				"hubspot_search_crm_objects",
				"SYSTEM_TIMEOUT:", // structural sentinel the system prompt matches on
				"TIMEOUT",         // load-bearing keyword
				"Do NOT name",     // explicit anti-confabulation instruction
				"Do NOT use the words denied or rejected",
				"the system expired the request",
			},
			// Critically: the load-bearing DENIED keyword must NOT
			// appear. The agent confabulated "Alice denied the
			// request" in production specifically because the timeout
			// path looked similar to the deny path; this guards that.
			// "approver" is also banned — the timeout branch must not
			// personify the outcome (no decision was made).
			mustNotContain: []string{"DENIED", "SYSTEM_DECISION_DENIED", "approver"},
		},
		{
			// REGRESSION CASE — production: channelsd's timeout watcher
			// publishes KindToolApprovalApplied with Decision="deny" +
			// Reason="timeout". The runner's NATS subscriber routes that
			// to DeliverDecision, so the awaiter wakes with awaitErr=nil
			// AND Decision{Approved:false, Reason:"timeout"}. Without
			// this guard the !d.Approved branch fired and the LLM saw
			// SYSTEM_DECISION_DENIED — exact path of the "Alice
			// denied" confabulation users hit twice in prod.
			name:     "timeout via channelsd Applied envelope (awaitErr nil, Reason=timeout)",
			toolName: "hubspot_search_crm_objects",
			decision: approval.Decision{Approved: false, Reason: "timeout"},
			awaitErr: nil,
			mustContain: []string{
				"hubspot_search_crm_objects",
				"SYSTEM_TIMEOUT:",
				"TIMEOUT",
				"Do NOT name",
			},
			mustNotContain: []string{"DENIED", "SYSTEM_DECISION_DENIED", "approver"},
		},
		{
			name:     "explicit deny: approver named",
			toolName: "hubspot_search_crm_objects",
			decision: approval.Decision{Approved: false, ApproverID: "user:Y29keS5iYXJkQGF1dGh6ZWQuY29t"},
			awaitErr: nil,
			mustContain: []string{
				"hubspot_search_crm_objects",
				"SYSTEM_DECISION_DENIED:",
				"DENIED",
				"user:Y29keS5iYXJkQGF1dGh6ZWQuY29t",
				"NOT a timeout",
			},
			mustNotContain: []string{"TIMEOUT", "SYSTEM_TIMEOUT"},
		},
		{
			name:     "deny with empty approver: still unambiguous",
			toolName: "hubspot_search_crm_objects",
			decision: approval.Decision{Approved: false},
			awaitErr: nil,
			mustContain: []string{
				"SYSTEM_DECISION_DENIED:",
				"DENIED",
				"hubspot_search_crm_objects",
				"(approver id not recorded)",
			},
			mustNotContain: []string{"TIMEOUT", "SYSTEM_TIMEOUT"},
		},
		{
			name:     "publish-failed: awaitErr but not a deadline (e.g. OnPublish returned)",
			toolName: "hubspot_search_crm_objects",
			decision: approval.Decision{},
			awaitErr: errors.New("publish approval request: nats: connection closed"),
			mustContain: []string{
				"SYSTEM_APPROVAL_ERROR:",
				"publish approval request",
				"hubspot_search_crm_objects",
				"NOT a denial",
			},
			mustNotContain: []string{"DENIED", "denied", "SYSTEM_TIMEOUT", "SYSTEM_DECISION_DENIED"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := approvalFailureContent(tc.toolName, tc.decision, tc.awaitErr)
			for _, sub := range tc.mustContain {
				assert.True(t, strings.Contains(got, sub),
					"expected content to contain %q (got %q)", sub, got)
			}
			for _, sub := range tc.mustNotContain {
				assert.False(t, strings.Contains(got, sub),
					"expected content NOT to contain %q (got %q)", sub, got)
			}
		})
	}
}
