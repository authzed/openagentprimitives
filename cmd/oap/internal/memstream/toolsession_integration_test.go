// cmd/oap/internal/memstream/toolsession_integration_test.go
//
// One end-to-end integration test for the tool_session feature path: the
// runner's persistence call (toolsession.Record) over an httpclient.Client
// -> httpsrv (real HTTP handler) -> memory.Local (operator facade) -> inmem
// Backend, then read back BOTH ways the CLI does it — toolsession.ReadAll
// for a one-shot fetch and memstream.StreamEntries for `--follow-live`.
//
// No mocks: the only fake is the in-memory Backend, the production-shaped
// Backend the operator runs today.
//
// This test lives here rather than in pkg/memory/httpsrv because the
// memstream package is cmd/oap/internal/... — Go's internal-package rule
// forbids pkg/memory/httpsrv from importing it. cmd/oap/internal/memstream
// is the only package from which httpclient, httpsrv, toolsession AND
// memstream are all reachable, so the full transport+storage+read-back
// round trip can only be exercised end to end from here.
package memstream_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/memstream"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register tool_session + sibling Kinds
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolsession"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// TestIntegration_ToolSession drives the whole tool_session feature path
// through the real stack. NOT parallel: it stands up an httptest.Server and
// keeps poll intervals short, so it should not contend with other tests for
// the shared transport.
//
// The three toolSessionLog modes (off / highSignal / full) decide WHICH
// events the runner's publisher hands to toolsession.Record — that gating
// (persistToolSessionEvent) is unit-tested in internal/cmd/runner. This test proves
// the layer below: whatever Record is called with reaches storage and reads
// back intact through both CLI read paths. It exercises the highSignal/full
// vocabulary (tool_use_start / tool_use_stop / result) end to end.
func TestIntegration_ToolSession(t *testing.T) {
	const (
		ns    = "default"
		name  = "ts-sess"
		token = "tok-toolsession"
	)
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}

	// Real stack: inmem.NewBackend -> memory.NewLocal -> httpsrv.NewHandler
	// -> httptest.Server. The blank import of kinds/all registered the
	// tool_session Kind, so Local accepts it.
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: ns, Name: name}, token, "")
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// httpclient.Client implements memory.Memory — the exact handle the
	// runner's tool_session publisher passes to toolsession.Record.
	client := httpclient.New(srv.URL, token)

	// The highSignal/full event vocabulary: a tool starts, the tool stops
	// OK, and the session ends with a terminal result carrying cost +
	// duration. Recorded one event apart so CreatedAt orders them — Record
	// stamps time.Now(), and ReadAll/fetchEntries sort ascending by it.
	events := []toolsession.Event{
		{
			ToolCallRef: "tc-1",
			EventType:   "tool_use_start",
			ToolName:    "bash",
			ToolID:      "toolu_01",
			Summary:     "run go test",
		},
		{
			ToolCallRef: "tc-1",
			EventType:   "tool_use_stop",
			ToolName:    "bash",
			ToolID:      "toolu_01",
			Summary:     "exit 0",
			OK:          true,
		},
		{
			ToolCallRef: "tc-1",
			EventType:   "result",
			OK:          true,
			DurationMs:  4200,
			CostUSD:     0.0137,
		},
	}

	// Persist via the public accessor the runner publisher calls — over the
	// httpclient, so the write crosses HTTP into the server's Local.
	for i, ev := range events {
		require.NoErrorf(t, toolsession.Record(ctx, client, scope, ev),
			"Record event %d (%s) over httpclient", i, ev.EventType)
		// Distinct CreatedAt per event so the ascending-by-time read order
		// is deterministic.
		time.Sleep(2 * time.Millisecond)
	}

	t.Run("ReadAll: one-shot accessor read-back over httpclient", func(t *testing.T) {
		// toolsession.ReadAll is the one-shot CLI fetch path (oap agent logs
		// without --follow-live).
		got, err := toolsession.ReadAll(ctx, client, scope)
		require.NoError(t, err, "ReadAll over httpclient")
		require.Len(t, got, 3, "all three recorded events returned")
		assert.Equal(t, events, got, "events round-trip with every field intact")

		// Spot-check the terminal result carried its numeric fields — these
		// are the ones --follow-live renders for the session summary.
		last := got[2]
		assert.Equal(t, "result", last.EventType)
		assert.True(t, last.OK, "result OK")
		assert.Equal(t, int64(4200), last.DurationMs, "result DurationMs")
		assert.InDelta(t, 0.0137, last.CostUSD, 1e-9, "result CostUSD")
	})

	t.Run("StreamEntries: --follow-live streaming read-back", func(t *testing.T) {
		// memstream.StreamEntries is the path `oap agent logs --follow-live`
		// uses. It polls GET /memory/tool_session/<ns>/<name> and emits each
		// newly-appeared Entry in ascending CreatedAt order.
		streamCtx, streamCancel := context.WithCancel(ctx)
		defer streamCancel()

		streamer := memstream.New(srv.URL, ns, name, token, 25*time.Millisecond)
		ch, errCh := streamer.StreamEntries(streamCtx, toolsession.KindName)

		var streamed []memory.Entry
		for e := range ch {
			streamed = append(streamed, e)
			if len(streamed) == 3 {
				// All three events received — stop the stream.
				streamCancel()
			}
		}
		if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("StreamEntries err: %v", err)
		}
		require.Len(t, streamed, 3, "all three events streamed")

		// Decode each Entry with toolsession.EntryToEvent — the decode the
		// CLI renderer applies — and assert it matches what was recorded.
		decoded := make([]toolsession.Event, 0, len(streamed))
		for i, e := range streamed {
			assert.Equalf(t, toolsession.KindName, e.Kind, "entry %d Kind", i)
			ev, derr := toolsession.EntryToEvent(e)
			require.NoErrorf(t, derr, "decode streamed entry %d", i)
			decoded = append(decoded, ev)
		}
		assert.Equal(t, events, decoded,
			"streamed events match what Record persisted, in CreatedAt order")
	})

	t.Run("server-side ID-prefix validation rejects a bad ID with HTTP 400", func(t *testing.T) {
		// A raw Put with an ID lacking the registered "toolsess-" prefix
		// must be rejected by Local.Put's validation server-side; the
		// handler maps that to HTTP 400, surfaced as an error by httpclient.
		_, err := client.Put(ctx, memory.Entry{
			Scope:     scope,
			Kind:      toolsession.KindName,
			ID:        "bad-no-prefix",
			CreatedAt: time.Now().UTC(),
			Content:   []byte(`{"toolCallRef":"tc-1","eventType":"result","ok":true}`),
		})
		require.Error(t, err, "Put with a bad tool_session ID prefix must fail")
		assert.Contains(t, err.Error(), "status=400",
			"server-side validation rejected the bad ID with HTTP 400")
	})
}
