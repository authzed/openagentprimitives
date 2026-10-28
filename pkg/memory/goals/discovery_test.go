package goals_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	goalsql "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlstore"
	"github.com/stretchr/testify/require"
)

func TestDiscoveryLedger(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			db := bridgeDB(t, backend)
			store := goalsql.New(db, backend == "postgres")
			require.NoError(t, store.Migrate(ctx))
			now := time.Now().UTC().Truncate(time.Microsecond)
			a := actor()
			tm := terms(a)
			tm.DueAt = now
			tm.ExpiresAt = now.Add(time.Hour)
			tm.Event = &goals.EventWatch{Source: sessionevents.Source{Kind: "native", Namespace: "team", ID: "team/source", UID: "source-original"}, Predicate: sessionevents.Predicate{Kind: "trip.changed", Subject: "approved-entity"}, MaxRuns: 2, RunWindowSeconds: 300, Timezone: "UTC", Burst: "skip_pending"}
			request := goals.DiscoveryRequest{RequestID: "policy", Title: "Trip monitor", Outcome: "Report trip changes privately", Terms: tm, Predicate: sessionevents.Predicate{Kind: "trip.upcoming", Subject: "inbox"}, SubjectField: "tripID", MaxProposals: 2, MaxPending: 1, ProposalSeconds: 60}
			p := goals.DiscoveryPolicy{ID: "policy", Actor: a, Request: request, Template: goals.Goal{ID: "policy", Domain: a.Domain, Revision: 1, Title: request.Title, Outcome: request.Outcome, State: goals.Active, OriginSession: a.Session, CreatedAt: now, UpdatedAt: now, Execution: &goals.ExecutionConsent{Purpose: "discovery_policy", Session: a.Session, SessionUID: a.SessionUID, RequestRevision: 1, Digest: "policy-digest", Terms: tm}}}
			p, err := store.CreateDiscoveryPolicy(ctx, p)
			require.NoError(t, err)
			page, err := store.List(ctx, a.Domain, goals.ListRequest{Limit: 100})
			require.NoError(t, err)
			require.Empty(t, page.Goals, "a policy is not a goal")
			witness, _ := json.Marshal(struct {
				CreatedAt time.Time `json:"createdAt"`
			}{now})
			decision := goals.ExecutionDecision{RequestID: "policy-approval", Owner: a.Domain.Owner, Digest: p.Template.Execution.Digest, Approved: true, Witness: string(witness)}
			p, err = store.DecideDiscoveryPolicy(ctx, p, decision)
			require.NoError(t, err)
			_, err = goals.DiscoverySubscription(p)
			require.NoError(t, err)
			makeProposal := func(event, subject string) goals.DiscoveryProposal {
				sub, err := goals.DiscoverySubscription(p)
				require.NoError(t, err)
				data, err := json.Marshal(map[string]string{"tripID": subject})
				require.NoError(t, err)
				o := sessionevents.Observation{Source: sub.Source, EventID: event, Kind: "trip.upcoming", Subject: "inbox", ObservedAt: now, Data: data, Dependencies: []sessionevents.Dependency{{ResourceType: "document", ResourceID: "private-trip", Permission: "view"}}}
				l := sessionevents.Launch{Subscription: sub, Observation: o, Admission: sessionevents.Admission{Disposition: "accepted", SubscriptionID: sub.ID, ObservationID: o.ID(), LaunchID: sessionevents.LaunchID(sub.ID, o.ID()), Window: sessionschedule.Window{DueAt: now, ExpiresAt: now.Add(time.Minute)}}}
				q, err := goals.NewDiscoveryProposal(p, l, now)
				require.NoError(t, err)
				return q
			}
			q, err := store.CreateDiscoveryProposal(ctx, p, makeProposal("first", "trip-1"))
			require.NoError(t, err)
			require.Equal(t, "pending", q.State)
			replay, err := store.CreateDiscoveryProposal(ctx, p, makeProposal("second-observation-same-trip", "trip-1"))
			require.NoError(t, err)
			require.Equal(t, q.ID, replay.ID, "entity suppression survives new observation identities")
			blocked, err := store.CreateDiscoveryProposal(ctx, p, makeProposal("second", "trip-2"))
			require.NoError(t, err)
			require.Equal(t, "suppressed", blocked.State, "pending limit is transactional")
			approve := goals.ExecutionDecision{RequestID: "exact-proposal-decision", Digest: q.Goal.Execution.Digest, Owner: a.Domain.Owner, Approved: true, Witness: string(witness)}
			changed := q
			changed.ExpiresAt = changed.ExpiresAt.Add(time.Second)
			_, err = store.DecideDiscoveryProposal(ctx, changed, approve, now)
			require.ErrorIs(t, err, goals.ErrDenied)
			var wg sync.WaitGroup
			failures := make(chan error, 8)
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := store.DecideDiscoveryProposal(ctx, q, approve, now.Add(time.Second))
					failures <- err
				}()
			}
			wg.Wait()
			close(failures)
			for err := range failures {
				require.NoError(t, err)
			}
			// Reopen the store object before replaying acceptance; no process state owns dedup.
			restarted := goalsql.New(db, backend == "postgres")
			accepted, err := restarted.DiscoveryProposal(ctx, a.Domain, q.ID)
			require.NoError(t, err)
			require.Equal(t, "accepted", accepted.State)
			_, err = restarted.DecideDiscoveryProposal(ctx, accepted, approve, now.Add(2*time.Second))
			require.NoError(t, err)
			contrary := approve
			contrary.Approved = false
			_, err = restarted.DecideDiscoveryProposal(ctx, accepted, contrary, now)
			require.ErrorIs(t, err, goals.ErrConflict)
			page, err = restarted.List(ctx, a.Domain, goals.ListRequest{Limit: 100})
			require.NoError(t, err)
			require.Len(t, page.Goals, 1)
			g := page.Goals[0]
			require.Equal(t, int64(2), g.Revision)
			require.Equal(t, q.ID, g.Discovery.ProposalID)
			require.Equal(t, approve, *g.Execution.Decision)
			require.Contains(t, g.Sources, goals.Source{ResourceType: "document", ResourceID: "private-trip", Permission: "view"})
			outbox, err := store.Pending(ctx, 100)
			require.NoError(t, err)
			require.Len(t, outbox, 1)
			require.Equal(t, "execution_decision", outbox[0].Action)
			next, err := store.CreateDiscoveryProposal(ctx, p, makeProposal("third", "trip-3"))
			require.NoError(t, err)
			require.Equal(t, "pending", next.State)
			deny := approve
			deny.RequestID = "decline-trip-3"
			deny.Digest = next.Goal.Execution.Digest
			deny.Approved = false
			declined, err := store.DecideDiscoveryProposal(ctx, next, deny, now.Add(time.Second))
			require.NoError(t, err)
			require.Equal(t, "declined", declined.State)
			extra, err := store.CreateDiscoveryProposal(ctx, p, makeProposal("fourth", "trip-4"))
			require.NoError(t, err)
			require.Equal(t, "suppressed", extra.State, "total notification limit does not reset after decline")
			var used int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT used FROM oap_discovery_policies WHERE id='policy'`).Scan(&used))
			require.Equal(t, 2, used)
			// A second reviewed policy proves expiration retains suppression and
			// cannot create a goal even when the original decision is replayed.
			expiring := p
			expiring.ID = "expiring-policy"
			expiring.Request.RequestID = "expiring-policy"
			expiring.Template.ID = expiring.ID
			consent := *expiring.Template.Execution
			expiring.Template.Execution = &consent
			expiring.Template.Execution.Digest = "expiring-policy-digest"
			expiring.Decision = nil
			expiring, err = store.CreateDiscoveryPolicy(ctx, expiring)
			require.NoError(t, err)
			policyDecision := decision
			policyDecision.RequestID = "approve-expiring-policy"
			policyDecision.Digest = expiring.Template.Execution.Digest
			expiring, err = store.DecideDiscoveryPolicy(ctx, expiring, policyDecision)
			require.NoError(t, err)
			oldPolicy := p
			p = expiring
			expires, err := store.CreateDiscoveryProposal(ctx, p, makeProposal("expired-observation", "expired-trip"))
			require.NoError(t, err)
			expiredDecision := approve
			expiredDecision.RequestID = "late-decision"
			expiredDecision.Digest = expires.Goal.Execution.Digest
			_, err = store.DecideDiscoveryProposal(ctx, expires, expiredDecision, expires.ExpiresAt)
			require.ErrorIs(t, err, goals.ErrDenied)
			suppressed, err := store.CreateDiscoveryProposal(ctx, p, makeProposal("same-expired-trip", "expired-trip"))
			require.NoError(t, err)
			require.Equal(t, expires.ID, suppressed.ID)
			_, err = store.DiscoveryProposal(ctx, goals.Domain{Namespace: "foreign", Owner: "foreign", Class: "assistant", ClassUID: "foreign"}, expires.ID)
			require.ErrorIs(t, err, goals.ErrNotFound)
			page, err = store.List(ctx, a.Domain, goals.ListRequest{Limit: 100})
			require.NoError(t, err)
			require.Len(t, page.Goals, 1)
			p = oldPolicy
			require.NoError(t, store.StopDiscoveryPolicy(ctx, a.Domain, p.ID))
			stopped, err := store.DiscoveryPolicy(ctx, a.Domain, p.ID)
			require.NoError(t, err)
			require.True(t, stopped.Stopped)
			_, err = store.CreateDiscoveryProposal(ctx, p, makeProposal("fifth", "trip-5"))
			require.ErrorIs(t, err, goals.ErrDenied)
		})
	}
}
