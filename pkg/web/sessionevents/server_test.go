package sessionevents

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	events "github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/stretchr/testify/require"
)

type ingressAdapter struct{ input events.Input }

func (a ingressAdapter) Kind() string { return "fixture" }
func (a ingressAdapter) Verify(context.Context, json.RawMessage) (events.Input, error) {
	return a.input, nil
}

type unavailableEventStore struct {
	events.Store
	err error
}

func (s unavailableEventStore) Checkpoint(context.Context, events.Source, string) (events.Checkpoint, error) {
	return events.Checkpoint{}, nil
}
func (s unavailableEventStore) Ingest(context.Context, events.Input, int64) (events.Observation, error) {
	return events.Observation{}, s.err
}

func TestEventIngressPersistenceFailureIsRetryable(t *testing.T) {
	input := events.Input{Publisher: "verified-component", Sequence: 1, Observation: events.Observation{
		Source: events.Source{Kind: "fixture", Namespace: "team", ID: "source", UID: "uid"}, EventID: "event", Kind: "changed", Subject: "trip", ObservedAt: time.Now().UTC(), Data: json.RawMessage(`{}`), Dependencies: []events.Dependency{{ResourceType: "document", ResourceID: "trip", Permission: "read"}},
	}}
	adapters := events.NewRegistry()
	adapters.Register(ingressAdapter{input})
	registry := tokens.NewRegistry()
	registry.SetChannelsdToken("component")
	for _, tc := range []struct {
		name  string
		err   error
		code  int
		retry string
	}{
		{"storage outage", errors.New("database unavailable"), http.StatusServiceUnavailable, "1"},
		{"conflicting replay", events.ErrConflict, http.StatusBadRequest, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &Server{Tokens: registry, Ingester: &events.Ingester{Adapters: adapters, Store: unavailableEventStore{err: tc.err}}}
			request := httptest.NewRequest(http.MethodPost, "/session-events/fixture", strings.NewReader(`{}`))
			request.Header.Set("Authorization", "Bearer component")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			require.Equal(t, tc.code, response.Code)
			require.Equal(t, tc.retry, response.Header().Get("Retry-After"))
		})
	}
}
