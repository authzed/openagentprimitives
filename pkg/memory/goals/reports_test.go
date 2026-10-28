package goals_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/stretchr/testify/require"
)

func TestReportedObservationSurvivesRunnerCleanup(t *testing.T) {
	for _, fixture := range durableFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.new(t)
			ledger := store.(goals.OccurrenceStore)
			runs := store.(goals.RunStore)
			outbox := store.(goals.ReportOutbox)
			svc, authority := executionService(store)
			a := actor()
			g, err := svc.Create(ctx, a, goals.CreateRequest{RequestID: "report-create", Title: "Flight collector", Outcome: "Report simulated flight changes"})
			require.NoError(t, err)
			g, err = svc.Update(ctx, a, goals.Change{RequestID: "report-activate", ID: g.ID, Revision: g.Revision, Action: "activate"})
			require.NoError(t, err)
			terms := terms(a)
			terms.AllowedOperations = []string{"report_goal_event"}
			terms.Report = &goals.ReportPolicy{Kind: "flight.changed", Subject: "FA1234"}
			g, err = svc.RequestExecution(ctx, a, goals.ExecutionRequest{RequestID: "report-request", ID: g.ID, Revision: g.Revision, Terms: terms})
			require.NoError(t, err)
			g, err = svc.DecideExecution(ctx, a.Domain, g.ID, goals.ExecutionDecision{RequestID: "report-approval", Digest: g.Execution.Digest, Owner: a.Domain.Owner, Approved: true, Witness: "test-human"})
			require.NoError(t, err)
			o, err := ledger.Schedule(ctx, g)
			require.NoError(t, err)
			now := executionNow.Add(2 * time.Minute)
			o, err = ledger.Claim(ctx, goals.ClaimRequest{ID: o.ID, Worker: "worker", Now: now, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 1})
			require.NoError(t, err)
			o, err = ledger.Attach(ctx, o, "runner-uid", now)
			require.NoError(t, err)
			proposal := goals.RunProposal{RequestID: "observed", Status: "reported_success", Summary: "Simulated delay", Sources: []goals.Source{{ResourceType: "document", ResourceID: "private-flight", Permission: "view"}}, Evidence: []string{"simulation"}, Observation: &goals.ObservationReport{Kind: "flight.changed", Subject: "other-flight", Data: json.RawMessage(`{"simulation":true,"status":"delayed"}`)}}
			_, err = runs.ProposeResult(ctx, o, proposal, now)
			require.ErrorIs(t, err, goals.ErrDenied)
			pending, err := outbox.PendingReportedObservations(ctx, 100)
			require.NoError(t, err)
			require.Empty(t, pending, "a rejected report must not create an outbox record")
			proposal.Observation.Subject = "FA1234"
			tooMany := proposal
			tooMany.Sources = nil
			for i := 0; i < 32; i++ {
				tooMany.Sources = append(tooMany.Sources, goals.Source{ResourceType: "document", ResourceID: fmt.Sprintf("doc-%d", i), Permission: "view"})
			}
			_, err = runs.ProposeResult(ctx, o, tooMany, now)
			require.ErrorIs(t, err, goals.ErrInvalid, "source constraints must be retained, never truncated")
			require.ErrorContains(t, err, "more than 32 source dependencies")

			o, err = runs.ProposeResult(ctx, o, proposal, now)
			require.NoError(t, err)
			_, err = runs.ProposeResult(ctx, o, proposal, now.Add(time.Second))
			require.NoError(t, err)
			o, err = runs.RecordOutcome(ctx, o, goals.RunSessionEnded, now)
			require.NoError(t, err)
			o, err = ledger.Finish(ctx, o, goals.OccurrenceFinished, now)
			require.NoError(t, err)
			due, err := ledger.Due(ctx, now, 100)
			require.NoError(t, err)
			require.Empty(t, due)
			pending, err = outbox.PendingReportedObservations(ctx, 100)
			require.NoError(t, err)
			require.Len(t, pending, 1, "publication survives terminal dispatch cleanup")
			adapter := &goals.ReportSource{Store: store, Runs: ledger, Auth: allow{}, Execution: authority}
			source := goals.ReportStream(g)
			raw, err := json.Marshal(goals.ReportReference{Source: source, OccurrenceID: o.ID})
			require.NoError(t, err)
			input, err := adapter.Verify(ctx, raw)
			require.NoError(t, err)
			require.Equal(t, proposal.Observation.Data, input.Observation.Data)
			require.Equal(t, "agent-reported-goal-result", input.Observation.Witness.Kind)
			require.Contains(t, input.Observation.Dependencies, sessionevents.Dependency{ResourceType: "document", ResourceID: "private-flight", Permission: "view"})
			require.Contains(t, input.Observation.Dependencies, sessionevents.Dependency{ResourceType: "agent_goal_domain", ResourceID: g.Domain.ID(), Permission: "view_memory"})
			require.ErrorIs(t, adapter.Check(ctx, "another-owner", source, nil), sessionevents.ErrDenied)
			wrong := source
			wrong.UID = "recreated"
			_, err = adapter.Resolve(ctx, a.Domain.Owner, wrong)
			require.ErrorIs(t, err, sessionevents.ErrDenied)
			// A transient authority failure does not acknowledge or lose the report.
			authority.grantErr = goals.ErrDenied
			_, err = adapter.Verify(ctx, raw)
			require.ErrorIs(t, err, goals.ErrDenied)
			pending, err = outbox.PendingReportedObservations(ctx, 100)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			authority.grantErr = nil
			require.NoError(t, outbox.AcknowledgeReportedObservation(ctx, o.ID))
			require.NoError(t, outbox.AcknowledgeReportedObservation(ctx, o.ID))
			pending, err = outbox.PendingReportedObservations(ctx, 100)
			require.NoError(t, err)
			require.Empty(t, pending)
			require.ErrorIs(t, outbox.AcknowledgeReportedObservation(ctx, "missing"), goals.ErrNotFound)
		})
	}
}
