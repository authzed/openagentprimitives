package goals_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory/goals/inmem"
	goalpostgres "github.com/authzed/openagentprimitives/pkg/memory/goals/postgres"
	mempostgres "github.com/authzed/openagentprimitives/pkg/memory/postgres"
	"github.com/authzed/openagentprimitives/test/testpostgres"
)

// domainStores are the three goal stores, each as a DomainListStore. The
// postgres fixture cleans its rows, since the shared container is not reset.
func domainStores() []struct {
	name string
	new  func(*testing.T) goals.Store
} {
	return []struct {
		name string
		new  func(*testing.T) goals.Store
	}{
		{"inmem", func(*testing.T) goals.Store { return inmem.New() }},
		{"sqlite", func(t *testing.T) goals.Store { return openSQL(t, filepath.Join(t.TempDir(), "goals.db")) }},
		{"postgres", func(t *testing.T) goals.Store {
			c, err := mempostgres.NewClient(context.Background(), testpostgres.URI(t))
			require.NoError(t, err)
			t.Cleanup(c.Close)
			s := goalpostgres.New(c.Pool())
			require.NoError(t, s.Migrate(context.Background()))
			t.Cleanup(func() {
				for _, q := range []string{"DELETE FROM oap_goal_execution_events", "DELETE FROM oap_goal_occurrences", "DELETE FROM oap_goal_events", "DELETE FROM oap_goal_receipts", "DELETE FROM oap_goals"} {
					_, err := c.Pool().Exec(context.Background(), q)
					assert.NoError(t, err)
				}
			})
			return s
		}},
	}
}

// TestListDomains: a read by domain-ID set returns only goals in those
// domains, in id order, paginated, and ignores IDs no goal is in.
func TestListDomains(t *testing.T) {
	domain := func(owner string) goals.Domain {
		return goals.Domain{Namespace: "team", Owner: owner, Class: "assistant", ClassUID: "class-uid"}
	}
	g1, g2, other := domain("alice"), domain("bob"), domain("carol")
	for _, fixture := range domainStores() {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := fixture.new(t)
			lister, ok := store.(goals.DomainListStore)
			require.True(t, ok, "the store reads by domain-ID set")
			s := service(store)
			made := map[string]goals.Domain{}
			for i, d := range []goals.Domain{g1, g1, g2, other} {
				a := actor()
				a.Domain = d
				g, err := s.Create(ctx, a, goals.CreateRequest{RequestID: fmt.Sprintf("create-%d", i), Title: "Standup", Outcome: "Summarize"})
				require.NoError(t, err)
				made[g.ID] = d
			}

			ids := []string{g1.ID(), g2.ID(), "no-such-domain"}
			var seen []string
			r := goals.ListRequest{Limit: 2}
			for {
				p, err := lister.ListDomains(ctx, ids, r)
				require.NoError(t, err)
				assert.LessOrEqual(t, len(p.Goals), 2, "a page never exceeds its limit")
				for _, g := range p.Goals {
					seen = append(seen, g.ID)
					assert.NotEqual(t, other, g.Domain, "a goal outside the requested domains is never returned")
				}
				if p.Next == "" {
					break
				}
				r.After = p.Next
			}
			assert.Len(t, seen, 3, "every goal in the requested domains, across pages")
			assert.IsIncreasing(t, seen, "in id order, so pages do not overlap")

			p, err := lister.ListDomains(ctx, []string{"no-such-domain"}, goals.ListRequest{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, p.Goals, "unknown domain IDs match nothing")
			p, err = lister.ListDomains(ctx, nil, goals.ListRequest{Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, p.Goals, "no domains, no goals")

			for id, d := range made {
				got, err := lister.GetInDomains(ctx, ids, id)
				if d == other {
					assert.ErrorIs(t, err, goals.ErrNotFound, "a goal outside the domains is not found")
					continue
				}
				require.NoError(t, err)
				assert.Equal(t, d, got.Domain)
			}
		})
	}
}
