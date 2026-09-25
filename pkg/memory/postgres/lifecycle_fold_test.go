package postgres_test

// lifecycle_fold_test.go exercises the postgres-backed lifecycle-log READ/FOLD
// path directly — the durability half of the unified-session-state restart
// thesis that the inmem-only e2e harness cannot reach.
//
// Context: the e2e harness hardwires an inmem lifecycle memory, so "the operator
// restarts, re-reads the durable log, and reconstructs phase" is structurally
// uncatchable there against a real durable backend. This test stands in for that
// at the storage layer: it writes a session's transition log to Postgres through
// one client, then re-reads and folds it through a SECOND client over the same
// database (modelling a restarted operator process with no in-process state).
// The log must survive the bounce and fold back to the live phase — it must not
// be lost (which, on an inmem backend, would demote a live Running session to
// the Pending bootstrap floor).
//
// Like every test in this package it requires POSTGRES_URI and skips cleanly
// when it is unset (the normal unit gate provisions no Postgres).

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	mempostgres "github.com/authzed/openagentprimitives/pkg/memory/postgres"
)

// openPostgresLifecycleMemory opens a fresh client over POSTGRES_URI, migrates,
// and returns a lifecycle-capable memory facade plus the client (closed by the
// caller). Constructing several of these over the same URI models successive
// operator processes reading the same durable log.
func openPostgresLifecycleMemory(t *testing.T, uri string) (*memory.Local, *mempostgres.Client) {
	t.Helper()
	ctx := context.Background()
	client, err := mempostgres.NewClient(ctx, uri)
	require.NoError(t, err, "connect to Postgres")
	require.NoError(t, client.Migrate(ctx), "migrate schema")
	return memory.NewLocal(mempostgres.NewBackend(client)), client
}

// TestPostgresLifecycleLog_SurvivesOperatorBounce_FoldsToRunning writes a
// mid-flight session's transition log (SettingsAccepted -> RunnerClaimed) to
// Postgres through one operator's memory facade, then re-reads and folds it
// through a fresh facade over the same database. The log must survive the bounce
// and fold to Running — proving a durable backend reconstructs a live session's
// phase across an operator restart, where inmem would lose the log and demote to
// Pending. Resolving through an open decision and back also round-trips durably.
func TestPostgresLifecycleLog_SurvivesOperatorBounce_FoldsToRunning(t *testing.T) {
	uri := os.Getenv("POSTGRES_URI")
	if uri == "" {
		t.Skip("POSTGRES_URI not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()

	const (
		ns   = "ns-bounce"
		name = "sess-bounce"
		uid  = "uid-bounce"
	)
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}

	// Clean the session's log up front and on teardown so reruns are isolated
	// (this package's other tests DELETE the whole table between cases; here we
	// scope the cleanup to our session's entries).
	cleanupMem, cleanupClient := openPostgresLifecycleMemory(t, uri)
	require.NoError(t, cleanupMem.DeleteScope(ctx, scope), "reset session scope before test")
	t.Cleanup(func() {
		_, _ = cleanupClient.Pool().Exec(context.Background(),
			"DELETE FROM memory_entry WHERE scope_kind = $1 AND scope_id = $2", scope.Kind, scope.ID)
		cleanupClient.Close()
	})

	runnerKey := func(turn, block int) lifecyclekind.OrderKey {
		return lifecyclekind.OrderKey{
			Seq:        channelevents.PackSeq(turn, block),
			Region:     string(lifecyclecore.RegionRunner),
			SessionUID: uid,
		}
	}

	// --- Operator process #1: write the mid-flight log to Postgres ----------
	writer, writerClient := openPostgresLifecycleMemory(t, uri)
	t.Cleanup(writerClient.Close)
	require.NoError(t, lifecyclekind.Append(ctx, writer, scope, lifecyclecore.SettingsAccepted{}, time.Now().UTC(), runnerKey(0, channelevents.SeqBlockStart)),
		"append SettingsAccepted")
	require.NoError(t, lifecyclekind.Append(ctx, writer, scope, lifecyclecore.RunnerClaimed{}, time.Now().UTC(), runnerKey(1, channelevents.SeqBlockStart)),
		"append RunnerClaimed")

	// --- Operator process #2 (the "bounce"): fresh facade, same database ----
	// A brand-new client with no in-process state re-reads the durable log.
	reader, readerClient := openPostgresLifecycleMemory(t, uri)
	t.Cleanup(readerClient.Close)

	events, err := lifecyclekind.Events(ctx, reader, scope)
	require.NoError(t, err, "re-read lifecycle log after bounce")
	require.Len(t, events, 2, "both transition events must survive the operator bounce (durable backend)")
	assert.Equal(t, string(lifecyclecore.PhaseRunning),
		lifecyclecore.Project(lifecyclecore.Fold(events)).Phase,
		"the bounced operator reconstructs Running from the durable log — a live session is not demoted to Pending")

	// The durability property that matters vs. inmem: the persisted log is
	// non-empty for a fresh reader. An inmem backend would return zero events
	// here, folding to the bootstrap Pending floor.
	assert.NotEmpty(t, events, "durable log must be non-empty for a freshly restarted operator")

	// --- An open decision + its resolution also survive durably -------------
	require.NoError(t, lifecyclekind.Append(ctx, reader, scope,
		lifecyclecore.DecisionAsked{RequestID: "r1", Kind: lifecyclecore.DecisionToolCall}, time.Now().UTC(), runnerKey(1, channelevents.SeqBlockStart+1)),
		"append DecisionAsked")

	pendingReader, pendingClient := openPostgresLifecycleMemory(t, uri)
	t.Cleanup(pendingClient.Close)
	pendingEvents, err := lifecyclekind.Events(ctx, pendingReader, scope)
	require.NoError(t, err, "re-read lifecycle log with open decision")
	assert.Equal(t, string(lifecyclecore.PhaseAwaitingDecision),
		lifecyclecore.Project(lifecyclecore.Fold(pendingEvents)).Phase,
		"the open decision survives the bounce and folds to AwaitingDecision")

	require.NoError(t, lifecyclekind.Append(ctx, pendingReader, scope,
		lifecyclecore.DecisionResolved{RequestID: "r1", Approved: true}, time.Now().UTC(), runnerKey(1, channelevents.SeqBlockStart+2)),
		"append DecisionResolved")

	resumedReader, resumedClient := openPostgresLifecycleMemory(t, uri)
	t.Cleanup(resumedClient.Close)
	resumedEvents, err := lifecyclekind.Events(ctx, resumedReader, scope)
	require.NoError(t, err, "re-read lifecycle log after resolution")
	assert.Equal(t, string(lifecyclecore.PhaseRunning),
		lifecyclecore.Project(lifecyclecore.Fold(resumedEvents)).Phase,
		"resolving the durable decision resumes the session to Running")
}
