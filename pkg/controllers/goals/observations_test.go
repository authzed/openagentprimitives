package goals

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	eventstore "github.com/authzed/openagentprimitives/pkg/memory/sessionevents/sqlstore"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/stretchr/testify/require"
)

func TestReportPublicationRetriesAfterCollectorFinishes(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "revoked"}[revoked], func(t *testing.T) { testReportPublication(t, revoked) })
	}
}
func testReportPublication(t *testing.T, revoked bool) {
	ctx := context.Background()
	db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "reports.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := goalsqlite.New(db.DB())
	require.NoError(t, store.Migrate(ctx))
	events := eventstore.New(db.DB(), false)
	require.NoError(t, events.Migrate(ctx))
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	auth := &runAuthority{}
	svc := &domain.Service{Store: store, Auth: auth, ExecutionAuth: auth, Now: func() time.Time { return now }}
	actor := domain.Actor{Domain: domain.Domain{Namespace: "team", Owner: "owner", Class: "collector", ClassUID: "class-uid"}, Session: "team/setup", Proof: "test-human"}
	g, err := svc.Create(ctx, actor, domain.CreateRequest{RequestID: "create", Title: "Flight collector", Outcome: "Publish simulated change"})
	require.NoError(t, err)
	g, err = svc.Update(ctx, actor, domain.Change{RequestID: "activate", ID: g.ID, Revision: g.Revision, Action: "activate"})
	require.NoError(t, err)
	g, err = svc.RequestExecution(ctx, actor, domain.ExecutionRequest{RequestID: "request", ID: g.ID, Revision: g.Revision, Terms: domain.ExecutionTerms{ClassDigest: "class", DueAt: now, ExpiresAt: now.Add(time.Hour), Bounds: domain.ExecutionBounds{DurationSeconds: 180, Turns: 10, Tokens: 10000, ApprovalSeconds: 90}, AllowedOperations: []string{"report_goal_event"}, Report: &domain.ReportPolicy{Kind: "flight.changed", Subject: "FA1234"}, Evidence: []string{"simulation"}, Destination: domain.PrivateDestination{Channel: "inbox", ChannelUID: "inbox-uid", Recipient: actor.Domain.Owner}}})
	require.NoError(t, err)
	g, err = svc.DecideExecution(ctx, actor.Domain, g.ID, domain.ExecutionDecision{RequestID: "approve", Digest: g.Execution.Digest, Owner: actor.Domain.Owner, Approved: true, Witness: "test-human"})
	require.NoError(t, err)
	o, err := store.Schedule(ctx, g)
	require.NoError(t, err)
	o, err = store.Claim(ctx, domain.ClaimRequest{ID: o.ID, Worker: "worker", Now: now, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 1})
	require.NoError(t, err)
	o, err = store.Attach(ctx, o, "collector-uid", now)
	require.NoError(t, err)
	o, err = store.ProposeResult(ctx, o, domain.RunProposal{RequestID: "result", Status: "reported_success", Summary: "Simulated delay", Evidence: []string{"simulation"}, Observation: &domain.ObservationReport{Kind: "flight.changed", Subject: "FA1234", Data: json.RawMessage(`{"simulation":true,"delayMinutes":60}`)}}, now)
	require.NoError(t, err)
	o, err = store.RecordOutcome(ctx, o, domain.RunSessionEnded, now)
	require.NoError(t, err)
	_, err = store.Finish(ctx, o, domain.OccurrenceFinished, now)
	require.NoError(t, err)
	registry := sessionevents.NewRegistry()
	registry.Register(&domain.ReportSource{Store: store, Runs: store, Auth: auth, Execution: auth})
	d := &Dispatcher{Service: svc, Store: store, EventIngester: &sessionevents.Ingester{Store: events, Adapters: registry}, Now: func() time.Time { return now }}
	auth.err = errors.New("temporary authorization outage")
	require.ErrorContains(t, d.drainReportedObservations(ctx), "temporary authorization outage")
	pending, err := store.PendingReportedObservations(ctx, 100)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	if revoked {
		auth.err = domain.ErrDenied
		require.ErrorIs(t, d.drainReportedObservations(ctx), domain.ErrDenied)
		pending, err = store.PendingReportedObservations(ctx, 100)
		require.NoError(t, err)
		require.Empty(t, pending, "permanent denials do not starve other publications")
		require.ErrorIs(t, store.AcknowledgeReportedObservation(ctx, o.ID), domain.ErrNotFound, "a racing acknowledgement cannot replace a discard")
		_, err = events.Get(ctx, domain.ReportStream(g), o.ID)
		require.ErrorIs(t, err, sessionevents.ErrNotFound)
		return
	}
	auth.err = nil
	// Simulate a crash between ingestion and the outbox acknowledgement.
	require.NoError(t, d.publishReportedObservation(ctx, o))
	require.NoError(t, d.drainReportedObservations(ctx))
	require.NoError(t, d.drainReportedObservations(ctx))
	pending, err = store.PendingReportedObservations(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, pending)
	observed, err := events.Get(ctx, domain.ReportStream(g), o.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{"simulation":true,"delayMinutes":60}`, string(observed.Data))
	checkpoint, err := events.Checkpoint(ctx, domain.ReportStream(g), "goal-run:"+o.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, checkpoint.Sequence)
}
