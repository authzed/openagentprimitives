package goals_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func requireSameCost(t *testing.T, expected, actual *goals.RunCost) {
	t.Helper()
	a, err := json.Marshal(expected)
	require.NoError(t, err)
	b, err := json.Marshal(actual)
	require.NoError(t, err)
	require.JSONEq(t, string(a), string(b))
}

func TestRunCostSnapshotsAreFencedAndDurable(t *testing.T) {
	for _, fixture := range durableFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			s, _ := executionService(fixture.new(t))
			g := approveExecution(t, s, actor(), "cost")
			ledger := s.Store.(goals.OccurrenceStore)
			costs := s.Store.(goals.CostStore)
			o, err := ledger.Schedule(ctx, g)
			require.NoError(t, err)
			now := executionNow.Add(2 * time.Minute)
			o, err = ledger.Claim(ctx, goals.ClaimRequest{ID: o.ID, Worker: "first", Now: now, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 4})
			require.NoError(t, err)
			o, err = ledger.Attach(ctx, o, "root-uid", now)
			require.NoError(t, err)
			estimate := &v1.EstimatedSessionCost{AmountMicroUSD: 1200, Currency: "USD", PricingKnown: false, AsOf: metav1.NewTime(now), ByModel: []v1.ModelCostBucket{{Model: "unknown-model", PricingKnown: false}}, ByTool: []v1.ToolCostBucket{{Tool: "inner-tool", AmountMicroUSD: 1200, PricingKnown: true}}}
			c := goals.RunCost{SessionUID: o.SessionUID, Estimate: estimate, Reason: "runner_active_or_interrupted"}
			captured, err := costs.RecordCost(ctx, o, c, now)
			require.NoError(t, err)
			require.Equal(t, int64(1200), captured.Cost.Estimate.AmountMicroUSD, "root total already contains the toolkit bucket")
			require.False(t, captured.Cost.Estimate.PricingKnown, "partial pricing cannot become known")
			repeated, err := costs.RecordCost(ctx, o, c, now.Add(time.Second))
			require.NoError(t, err)
			requireSameCost(t, captured.Cost, repeated.Cost)
			events, err := s.Store.Pending(ctx, 100)
			require.NoError(t, err)
			costEvents := 0
			for _, event := range events {
				if event.Action == "execution_cost_observed" && event.Occurrence != nil && event.Occurrence.ID == o.ID {
					costEvents++
				}
			}
			require.Equal(t, 1, costEvents, "duplicate accounting observations must not grow the audit outbox")
			wrong := o
			wrong.SessionUID = "another-root"
			_, err = costs.RecordCost(ctx, wrong, goals.RunCost{SessionUID: wrong.SessionUID, Reason: "missing"}, now)
			require.ErrorIs(t, err, goals.ErrConflict)
			_, err = costs.RecordCost(ctx, o, goals.RunCost{SessionUID: o.SessionUID, Reason: "missing"}, now)
			require.ErrorIs(t, err, goals.ErrConflict, "session disappearance cannot erase spend")
			stale := *estimate.DeepCopy()
			stale.AsOf = metav1.NewTime(now.Add(-time.Second))
			_, err = costs.RecordCost(ctx, o, goals.RunCost{SessionUID: o.SessionUID, Estimate: &stale}, now)
			require.ErrorIs(t, err, goals.ErrConflict)
			regressed := estimate.DeepCopy()
			regressed.AsOf = metav1.NewTime(now.Add(time.Second))
			regressed.AmountMicroUSD = 100
			_, err = costs.RecordCost(ctx, o, goals.RunCost{SessionUID: o.SessionUID, Estimate: regressed}, now.Add(time.Second))
			require.ErrorIs(t, err, goals.ErrConflict, "newer timestamps cannot erase previously observed spend")
			takeover, err := ledger.Claim(ctx, goals.ClaimRequest{ID: o.ID, Worker: "next", Now: now.Add(time.Minute), Lease: time.Minute, OwnerLimit: 1, ClassLimit: 4})
			require.NoError(t, err)
			c.Final = true
			c.Reason = ""
			_, err = costs.RecordCost(ctx, o, c, now.Add(time.Minute))
			require.ErrorIs(t, err, goals.ErrConflict)
			final, err := costs.RecordCost(ctx, takeover, c, now.Add(time.Minute))
			require.NoError(t, err)
			require.True(t, final.Cost.Final)
			c.Final = false
			_, err = costs.RecordCost(ctx, takeover, c, now.Add(time.Minute))
			require.ErrorIs(t, err, goals.ErrConflict, "a final snapshot cannot be downgraded")
			_, err = ledger.Finish(ctx, takeover, goals.OccurrenceFinished, now.Add(time.Minute))
			require.NoError(t, err)
			page, err := s.Runs(ctx, actor(), g.ID, goals.ListRequest{Limit: 10})
			require.NoError(t, err)
			requireSameCost(t, final.Cost, page.Runs[0].Cost)
			got, err := s.Get(ctx, actor(), g.ID)
			require.NoError(t, err)
			require.Equal(t, goals.Active, got.State, "accounting does not complete a goal")
		})
	}
}

func TestRunCostSurvivesSQLiteReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "costs.db")
	db, err := memsqlite.NewClient(path)
	require.NoError(t, err)
	original := goalsqlite.New(db.DB())
	require.NoError(t, original.Migrate(ctx))
	s, _ := executionService(original)
	g := approveExecution(t, s, actor(), "cost-reopen")
	ledger := s.Store.(goals.OccurrenceStore)
	o, err := ledger.Schedule(ctx, g)
	require.NoError(t, err)
	now := executionNow.Add(2 * time.Minute)
	o, err = ledger.Claim(ctx, goals.ClaimRequest{ID: o.ID, Worker: "worker", Now: now, Lease: time.Minute, OwnerLimit: 1, ClassLimit: 4})
	require.NoError(t, err)
	o, err = ledger.Attach(ctx, o, "root-uid", now)
	require.NoError(t, err)
	// Upgrade an existing version-4 ledger; its root identity must survive.
	for _, q := range []string{"DROP TABLE oap_goal_run_costs", "DELETE FROM oap_goal_schema", "INSERT INTO oap_goal_schema(version) VALUES(4)"} {
		_, err = db.DB().ExecContext(ctx, q)
		require.NoError(t, err)
	}
	require.NoError(t, original.Migrate(ctx))
	require.NoError(t, original.Migrate(ctx))
	migrated, err := ledger.Occurrence(ctx, o.ID)
	require.NoError(t, err)
	require.Equal(t, o.SessionUID, migrated.SessionUID)
	require.Nil(t, migrated.Cost)
	before, err := s.Store.(goals.CostStore).RecordCost(ctx, o, goals.RunCost{SessionUID: o.SessionUID, Reason: "estimate_unavailable"}, now)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	reopened := openSQL(t, path)
	require.NoError(t, reopened.(interface{ Migrate(context.Context) error }).Migrate(ctx))
	after, err := reopened.(goals.OccurrenceStore).Occurrence(ctx, o.ID)
	require.NoError(t, err)
	requireSameCost(t, before.Cost, after.Cost)
	require.Nil(t, after.Cost.Estimate, "missing pricing must not become a zero-cost estimate")
}
