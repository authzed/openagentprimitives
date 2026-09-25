// pkg/memory/httpsrv/integration_test.go
//
// End-to-end integration of the v2 memory stack: httpclient (client) ↔
// httpsrv (real HTTP handler over an httptest.Server) ↔ memory.Local
// (operator-side facade + inmem backend + Kind hooks). No mocks — the
// only fake is the in-memory backend, which is the production-shaped
// Backend the operator runs today.
package httpsrv_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// TestIntegration_V2Stack drives the full client→server→Local path. It is
// NOT parallel: lifecycleSetup mutates the lifecycle package's global
// facade reference, so concurrent tests touching it would race.
//
// Reuses newTestServer + lifecycleSetup from httpsrv_test.go (real
// httpsrv.NewHandler over memory.NewLocal(inmem.NewBackend())).
func TestIntegration_V2Stack(t *testing.T) {
	const (
		ns    = "ns"
		name  = "n"
		token = "tok-integration"
	)
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}

	// Mint a per-session token the handler accepts for this scope.
	srv, mem, reg := newTestServer(t)
	reg.Set(memory.NamespacedName{Namespace: ns, Name: name}, token, "")

	// Wire the lifecycle hook to the same Local the server holds, so a
	// signal crossing HTTP triggers a write inside the server's Local.
	lifecycleSetup(t, mem)

	// The runner/ap/channelsd path: a Client implementing memory.Memory.
	client := httpclient.New(srv.URL, token)
	ctx := context.Background()

	t.Run("turn round-trip and server-side validation", func(t *testing.T) {
		// Put a valid turn Entry via the client, then Query it back.
		good := turnEntry(turn.EntryID(0, "user"), "round-trip")
		good.Scope = scope
		stored, err := client.Put(ctx, good)
		require.NoError(t, err, "Put valid turn Entry")
		assert.Equal(t, turn.EntryID(0, "user"), stored.ID, "stored ID")
		assert.Equal(t, scope, stored.Scope, "scope forced from URL")

		res, err := client.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"turn"}})
		require.NoError(t, err, "Query turn Kind")
		require.Len(t, res.Entries, 1, "one turn entry round-tripped")
		assert.Equal(t, turn.EntryID(0, "user"), res.Entries[0].ID, "queried entry ID")
		assert.JSONEq(t, string(good.Content), string(res.Entries[0].Content),
			"queried entry content matches what was Put")

		// Put a turn Entry whose ID lacks the registered "turn-" prefix.
		// Local.Put validation must reject it server-side; the handler
		// maps that to HTTP 400, which the client surfaces as an error.
		bad := turnEntry("not-a-turn-id", "bad-prefix")
		bad.Scope = scope
		_, err = client.Put(ctx, bad)
		require.Error(t, err, "Put with a bad ID prefix must fail")
		assert.Contains(t, err.Error(), "status=400",
			"server-side validation rejected the bad ID with HTTP 400")
	})

	t.Run("signal over HTTP triggers the lifecycle hook server-side", func(t *testing.T) {
		// SendSignal crosses the HTTP boundary; the handler dispatches it
		// to the server's Local, whose lifecycle ScopeHooks writes an entry.
		err := client.SendSignal(ctx, memory.Signal{
			Scope: scope,
			Kind:  lifecycle.SigSessionCompleted,
			At:    time.Unix(0, 0).UTC(),
		})
		require.NoError(t, err, "SendSignal over HTTP")

		res, err := client.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"lifecycle"}})
		require.NoError(t, err, "Query lifecycle Kind")
		require.Len(t, res.Entries, 1, "lifecycle hook wrote one entry inside the server's Local")
		assert.Contains(t, res.Entries[0].Tags, lifecycle.SignalTag(lifecycle.SigSessionCompleted),
			"lifecycle entry tagged with the signal kind, inside the signal namespace")
	})

	t.Run("turn.Appender end-to-end over httpclient (runner path)", func(t *testing.T) {
		// The runner's real transcript path: turn.Appender over a
		// memory.Memory that happens to be an httpclient.Client. Use a
		// distinct session/scope (and its own token) so the round-trip
		// subtest's turn-0-user entry doesn't collide here.
		const appName = "appsess"
		appScope := memory.Scope{Kind: "session", ID: ns + "/" + appName}
		const appToken = "tok-appsess"
		reg.Set(memory.NamespacedName{Namespace: ns, Name: appName}, appToken, "")
		appClient := httpclient.New(srv.URL, appToken)
		app := turn.NewAppender(appClient, appScope)

		// Append out of order to prove ReadAll sorts by (index, role).
		turns := []memory.Turn{
			{
				Index:     2,
				Role:      "system_note",
				Content:   []memory.ContentBlock{{Type: "text", Text: "operator note"}},
				CreatedAt: time.Unix(2, 0).UTC(),
			},
			{
				Index:     0,
				Role:      "user",
				Content:   []memory.ContentBlock{{Type: "text", Text: "hello"}},
				CreatedAt: time.Unix(0, 0).UTC(),
			},
			{
				Index:     1,
				Role:      "assistant",
				Content:   []memory.ContentBlock{{Type: "text", Text: "hi there"}},
				CreatedAt: time.Unix(1, 0).UTC(),
				Usage:     &memory.Usage{InputTokens: 7, OutputTokens: 11},
			},
		}
		for _, tn := range turns {
			require.NoErrorf(t, app.Append(ctx, tn), "Append turn %d/%s", tn.Index, tn.Role)
		}

		got, err := app.ReadAll(ctx)
		require.NoError(t, err, "ReadAll over httpclient")
		require.Len(t, got, 3, "all three appended turns returned")

		assert.Equal(t, 0, got[0].Index)
		assert.Equal(t, "user", got[0].Role)
		assert.Equal(t, turns[1].Content, got[0].Content)

		assert.Equal(t, 1, got[1].Index)
		assert.Equal(t, "assistant", got[1].Role)
		assert.Equal(t, turns[2].Content, got[1].Content)
		require.NotNil(t, got[1].Usage, "assistant turn must round-trip Usage")
		assert.Equal(t, *turns[2].Usage, *got[1].Usage)

		assert.Equal(t, 2, got[2].Index)
		assert.Equal(t, "system_note", got[2].Role)
		assert.Equal(t, turns[0].Content, got[2].Content)
	})
}
