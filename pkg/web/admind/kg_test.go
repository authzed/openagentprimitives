package admind_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
)

// fakeKG is a scripted memory.KGProvider for handler tests: every read method
// returns the configured slice/pointer, or the configured error when err is set.
type fakeKG struct {
	facts       []memory.KGFact
	entity      *memory.KGEntity
	entities    []memory.KGEntity
	communities []memory.KGCommunity
	err         error
}

func (f fakeKG) Ingest(context.Context, memory.KGInput) error { return nil }
func (f fakeKG) SearchFacts(context.Context, string, int) ([]memory.KGFact, error) {
	return f.facts, f.err
}
func (f fakeKG) GetEntity(context.Context, string) (*memory.KGEntity, error) {
	return f.entity, f.err
}
func (f fakeKG) EntityFacts(context.Context, string) ([]memory.KGFact, error) {
	return f.facts, f.err
}
func (f fakeKG) RelatedEntities(context.Context, string, int) ([]memory.KGEntity, error) {
	return f.entities, f.err
}
func (f fakeKG) Communities(context.Context, string) ([]memory.KGCommunity, error) {
	return f.communities, f.err
}

// newKGAdmind builds an admind whose Config.KG is the given provider (nil =
// KG unavailable). The admin subject "YWRtaW4" is allowed for every permission.
func newKGAdmind(t *testing.T, kg memory.KGProvider) *admind.Admind {
	t.Helper()
	a, err := admind.New(admind.Config{
		Mem:     memory.NewLocal(inmem.NewBackend()),
		K8s:     fake.NewClientBuilder().WithScheme(newScheme(t)).Build(),
		Checker: stubChecker{allow: map[string]bool{"YWRtaW4": true}},
		Token:   "test-token",
		Logger:  testr.New(t),
		KG:      kg,
	})
	require.NoError(t, err)
	return a
}

func TestAdmindKG_Unavailable(t *testing.T) {
	// No KGProvider wired → every action degrades to available:false 200,
	// never a 500. The frontend renders a clean "not configured" panel.
	h := newKGAdmind(t, nil).Handler()
	for _, action := range []string{"search", "entity", "facts", "related", "communities"} {
		w := do(t, h, http.MethodGet, "/admin/v1/kg/"+action, "test-token", "user:YWRtaW4", "")
		require.Equal(t, http.StatusOK, w.Code, "action %s must not 500 when KG is off", action)
		var got struct {
			Available bool   `json:"available"`
			Note      string `json:"note"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.False(t, got.Available, "available:false when KG off")
		assert.NotEmpty(t, got.Note, "degraded note set")
	}
}

func TestAdmindKG_Actions(t *testing.T) {
	validAt := mustTime(t, "2026-06-10T12:00:00Z")
	kg := fakeKG{
		facts: []memory.KGFact{
			{UUID: "f1", Name: "deployed", Fact: "alice deployed infra", FromEntity: "e1", ToEntity: "e2", ValidAt: &validAt},
		},
		entity:   &memory.KGEntity{UUID: "e1", Name: "alice@example.com", Summary: "a person"},
		entities: []memory.KGEntity{{UUID: "e2", Name: "repo:infra", Summary: "a resource"}},
		communities: []memory.KGCommunity{
			{UUID: "c1", Name: "infra-team", Summary: "infra cluster", Members: []string{"e1", "e2"}},
		},
	}
	h := newKGAdmind(t, kg).Handler()

	t.Run("search: available:true with facts", func(t *testing.T) {
		w := do(t, h, http.MethodGet, "/admin/v1/kg/search?q=deploy", "test-token", "user:YWRtaW4", "")
		require.Equal(t, http.StatusOK, w.Code)
		var got struct {
			Available bool            `json:"available"`
			Facts     []memory.KGFact `json:"facts"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.True(t, got.Available)
		require.Len(t, got.Facts, 1)
		assert.Equal(t, "alice deployed infra", got.Facts[0].Fact)
	})

	t.Run("entity: returns the entity", func(t *testing.T) {
		w := do(t, h, http.MethodGet, "/admin/v1/kg/entity?uuid=e1", "test-token", "user:YWRtaW4", "")
		require.Equal(t, http.StatusOK, w.Code)
		var got struct {
			Available bool             `json:"available"`
			Entity    *memory.KGEntity `json:"entity"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.True(t, got.Available)
		require.NotNil(t, got.Entity)
		assert.Equal(t, "alice@example.com", got.Entity.Name)
	})

	t.Run("facts: returns the entity's facts", func(t *testing.T) {
		w := do(t, h, http.MethodGet, "/admin/v1/kg/facts?uuid=e1", "test-token", "user:YWRtaW4", "")
		require.Equal(t, http.StatusOK, w.Code)
		var got struct {
			Available bool            `json:"available"`
			Facts     []memory.KGFact `json:"facts"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		require.Len(t, got.Facts, 1)
	})

	t.Run("related: returns related entities", func(t *testing.T) {
		w := do(t, h, http.MethodGet, "/admin/v1/kg/related?uuid=e1", "test-token", "user:YWRtaW4", "")
		require.Equal(t, http.StatusOK, w.Code)
		var got struct {
			Available bool              `json:"available"`
			Entities  []memory.KGEntity `json:"entities"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		require.Len(t, got.Entities, 1)
		assert.Equal(t, "repo:infra", got.Entities[0].Name)
	})

	t.Run("communities: returns communities", func(t *testing.T) {
		w := do(t, h, http.MethodGet, "/admin/v1/kg/communities", "test-token", "user:YWRtaW4", "")
		require.Equal(t, http.StatusOK, w.Code)
		var got struct {
			Available   bool                 `json:"available"`
			Communities []memory.KGCommunity `json:"communities"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		require.Len(t, got.Communities, 1)
		assert.Equal(t, "infra-team", got.Communities[0].Name)
	})

	t.Run("unknown action → 404", func(t *testing.T) {
		w := do(t, h, http.MethodGet, "/admin/v1/kg/bogus", "test-token", "user:YWRtaW4", "")
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestAdmindKG_ProviderErrorIsDegradedNot500(t *testing.T) {
	// A provider error mid-request surfaces as 200 {available:true, error:...}
	// — a degraded result the panel can show, not a blank 500.
	h := newKGAdmind(t, fakeKG{err: errors.New("graphiti unreachable")}).Handler()
	w := do(t, h, http.MethodGet, "/admin/v1/kg/search?q=x", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var got struct {
		Available bool            `json:"available"`
		Facts     []memory.KGFact `json:"facts"`
		Error     string          `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.True(t, got.Available, "still available; just degraded")
	assert.NotEmpty(t, got.Error, "the provider error is surfaced")
	assert.NotNil(t, got.Facts, "facts is a non-nil empty array")
}

func TestAdmindKG_AuthGate(t *testing.T) {
	// view_audit gates the route: a non-admin subject is forbidden.
	h := newKGAdmind(t, fakeKG{}).Handler()
	w := do(t, h, http.MethodGet, "/admin/v1/kg/search", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "kg needs view_audit")
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return ts
}
