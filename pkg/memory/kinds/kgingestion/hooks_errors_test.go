package kgingestion_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/kgingestion"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// stubMemory answers Query with whatever the test wants, including a failure.
type stubMemory struct {
	res memory.QueryResult
	err error
}

func (s *stubMemory) Put(_ context.Context, e memory.Entry) (memory.Entry, error) { return e, nil }
func (s *stubMemory) Query(_ context.Context, _ memory.Query) (memory.QueryResult, error) {
	return s.res, s.err
}
func (s *stubMemory) Search(_ context.Context, _ memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}
func (s *stubMemory) SendSignal(_ context.Context, _ memory.Signal) error { return nil }

// captureLogs redirects the package-level slog default for the duration of
// one test. Mutates process-global state, so these tests must not be parallel.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func turnCompletedSignal(t *testing.T, scope memory.Scope, payload any) memory.Signal {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return memory.Signal{Scope: scope, Kind: lifecycle.SigTurnCompleted, Payload: raw}
}

func hooksOver(t *testing.T, m memory.Memory, scope memory.Scope) memory.ScopeHooks {
	t.Helper()
	t.Cleanup(kgingestion.Teardown)
	kgingestion.Setup(m, &fakeKGProvider{}, kgingestion.IngestionConfig{})
	return kgingestion.Kind{}.NewScopeHooks(scope)
}

// A failed turn read and a legitimate "no such turn" both produced the same
// empty string, so a memory outage looked exactly like a turn that was never
// written: ingestion stopped for the whole session with nothing in the log to
// say why. The sibling ingest path has always logged its failures.
func TestKGIngestion_TurnReadFailureIsLogged(t *testing.T) {
	logs := captureLogs(t)
	scope := memory.Scope{Kind: "session", ID: "ns/test"}
	h := hooksOver(t, &stubMemory{err: errors.New("memory backend unreachable")}, scope)

	err := h.OnSignal(context.Background(), turnCompletedSignal(t, scope,
		map[string]string{"turnIndex": "3", "role": "assistant"}))

	assert.NoError(t, err, "an optional enrichment must not fail the caller's signal")
	assert.Contains(t, logs.String(), "memory backend unreachable",
		"the read failure must reach the log")
}

// A turn whose content will not decode is a corrupt record, not an absent
// one; dropping it silently hides the corruption.
func TestKGIngestion_UndecodableTurnIsLogged(t *testing.T) {
	logs := captureLogs(t)
	scope := memory.Scope{Kind: "session", ID: "ns/test"}
	h := hooksOver(t, &stubMemory{res: memory.QueryResult{
		Entries: []memory.Entry{{
			Scope: scope, Kind: "turn", ID: "turn-000003-assistant",
			Content: []byte("{not json"),
		}},
	}}, scope)

	err := h.OnSignal(context.Background(), turnCompletedSignal(t, scope,
		map[string]string{"turnIndex": "3", "role": "assistant"}))

	assert.NoError(t, err)
	assert.Contains(t, logs.String(), "turn-000003-assistant",
		"the undecodable entry must be named in the log")
}

// A signal whose payload will not decode silently ingested turn 0 of role ""
// — a turn that does not exist — so the real turn was never ingested.
func TestKGIngestion_UndecodableSignalPayloadIsLogged(t *testing.T) {
	logs := captureLogs(t)
	scope := memory.Scope{Kind: "session", ID: "ns/test"}
	h := hooksOver(t, &stubMemory{}, scope)

	err := h.OnSignal(context.Background(), memory.Signal{
		Scope: scope, Kind: lifecycle.SigTurnCompleted, Payload: []byte("{not json"),
	})

	assert.NoError(t, err)
	assert.Contains(t, logs.String(), "kg ingestion",
		"the undecodable signal payload must reach the log")
}

// A turn that genuinely is not there yet is not an error and must stay quiet.
func TestKGIngestion_MissingTurnLogsNothing(t *testing.T) {
	logs := captureLogs(t)
	scope := memory.Scope{Kind: "session", ID: "ns/test"}
	h := hooksOver(t, &stubMemory{}, scope)

	err := h.OnSignal(context.Background(), turnCompletedSignal(t, scope,
		map[string]string{"turnIndex": "3", "role": "assistant"}))

	assert.NoError(t, err)
	assert.Empty(t, logs.String())
}
