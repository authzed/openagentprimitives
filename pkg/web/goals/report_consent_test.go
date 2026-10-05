package goals

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/stretchr/testify/require"
)

func TestCollectorConsentDisclosesExactReportingScope(t *testing.T) {
	now := time.Now().UTC()
	for _, reply := range []bool{false, true} {
		g := domain.Goal{ID: "collector", Title: "Flight collector", Domain: domain.Domain{Namespace: "team", Owner: "YWxpY2VAZXhhbXBsZS5jb20", Class: "collector", ClassUID: "uid"}, Execution: &domain.ExecutionConsent{Session: "team/setup", Digest: "review", Terms: domain.ExecutionTerms{ActionApproval: "standing_private", DueAt: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour), Bounds: domain.ExecutionBounds{DurationSeconds: 180, Turns: 10, Tokens: 10000, ApprovalSeconds: 90}, Report: &domain.ReportPolicy{Kind: "flight.changed", Subject: "FA1234"}, AllowedOperations: []string{"report_goal_event"}}}}
		if reply {
			g.Execution.Terms.AllowedOperations = append(g.Execution.Terms.AllowedOperations, "respond_to_user")
		}
		entry, err := consentEntry(g, now)
		require.NoError(t, err)
		var content goalconsent.Content
		require.NoError(t, json.Unmarshal(entry.Content, &content))
		var card channelevents.InteractionRequestPayload
		require.NoError(t, json.Unmarshal(content.Request, &card))
		require.Equal(t, "Allow this private observation collector?", card.Lead)
		require.Contains(t, card.Body, "do not independently verify external facts")
		var actions string
		for _, field := range card.Fields {
			if field.Label == "Permitted action" {
				actions = field.Value
			}
		}
		require.Contains(t, actions, "FA1234")
		require.Equal(t, reply, strings.Contains(actions, "Send a private report to you"))
		require.Equal(t, g.Execution.Terms, content.Goal.Execution.Terms)
	}
}
