package meta_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observation"
)

// fakePoolWriter is record_observation's sess.Mem double. It satisfies
// tool.MemoryQuerier trivially (Query/Search go unused by this tool) and
// PutToPool via an injected func, so a test can observe exactly what pool and
// entry Execute handed to the write path — or exercise a server refusal by
// having put return an error.
type fakePoolWriter struct {
	put func(ctx context.Context, sessionScope, poolScope memory.Scope, e memory.Entry) (memory.Entry, error)
}

func (f fakePoolWriter) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}

func (f fakePoolWriter) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}

func (f fakePoolWriter) PutToPool(ctx context.Context, sessionScope, poolScope memory.Scope, e memory.Entry) (memory.Entry, error) {
	if f.put == nil {
		// A nil put is used ONLY by tests asserting a refusal that must never
		// reach the write path (malformed resource, empty text). Panicking
		// rather than returning a graceful error makes a would-be-masked
		// mutation (the validation it exists to gate quietly removed) loud
		// instead of indistinguishable from a legitimate write error, which
		// would also set IsError true and let the mutation pass unnoticed.
		panic("fakePoolWriter: PutToPool called with no put func wired — the call should have been refused before reaching the write path")
	}
	return f.put(ctx, sessionScope, poolScope, e)
}

// sessionWithPoolWriter builds a *tool.SessionContext whose Mem is a
// fakePoolWriter over put — nil is valid for tests that must never reach the
// write path (argument validation refusals).
func sessionWithPoolWriter(put func(ctx context.Context, sessionScope, poolScope memory.Scope, e memory.Entry) (memory.Entry, error)) *tool.SessionContext {
	return &tool.SessionContext{Namespace: "default", Name: "s1", Mem: fakePoolWriter{put: put}}
}

// TestRecordObservationClaimsNoAuthorizationOfItsOwn pins the DECLARATION, and
// says only that. Whether the call then reaches the pipeline, and what the
// pipeline answers, is proved by running the dispatcher's predicate and the
// real tool-call gate over this declaration — in
// pkg/agent/runner/record_observation_routing_test.go, the only package that
// can see both.
//
// The name matters. This test replaces one called
// TestRecordObservationRoutesThroughThePipeline, which asserted
// `StateImpact == Readwrite && Check == nil` — the pair that made routing end
// in a deny on every call — while its name read as a coverage claim about
// routing. A reader auditing coverage ticked the box.
func TestRecordObservationClaimsNoAuthorizationOfItsOwn(t *testing.T) {
	perm := meta.NewRecordObservation().Permission()
	assert.Equal(t, authz.Stateless, perm.StateImpact,
		"the destination type varies per call, so no static Check can express it — and a check-requiring StateImpact with a nil Check is denied outright by toolcheck.Checker")
	assert.False(t, perm.StateImpact.CheckRequired())
	assert.Nil(t, perm.Check)
}

func TestRecordObservationWritesToTheNamedPool(t *testing.T) {
	var gotPool memory.Scope
	var gotEntry memory.Entry
	sess := sessionWithPoolWriter(func(_ context.Context, _, pool memory.Scope, e memory.Entry) (memory.Entry, error) {
		gotPool, gotEntry = pool, e
		return e, nil
	})

	res, err := meta.NewRecordObservation().Execute(context.Background(),
		json.RawMessage(`{"resource":"dossier:d-1","text":"prefers async review","tags":["process"]}`), sess)

	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "resource", gotPool.Kind)
	assert.Equal(t, "dossier:d-1", gotPool.ID)
	assert.Equal(t, observation.KindName, gotEntry.Kind)
	assert.Equal(t, []string{"process"}, gotEntry.Tags)

	// The entry handed to PutToPool must already carry an ID and a non-zero
	// CreatedAt. EntryDigest signs both (pkg/memory/provenance/digest.go), and
	// httpsrv assigns its own ID when the body has none — an entry that
	// reaches the wire without one signs a digest the server-forced entry can
	// never re-verify, which is exactly the blocker this pins.
	assert.NotEmpty(t, gotEntry.ID, "an unset ID signs a digest verify-on-write can never match once the server assigns one")
	assert.False(t, gotEntry.CreatedAt.IsZero(), "a zero CreatedAt persists forever, breaking newest-first ordering and Since/Until filters")

	var c observation.Content
	require.NoError(t, json.Unmarshal(gotEntry.Content, &c))
	assert.Equal(t, "prefers async review", c.Text)
}

func TestRecordObservationRefusesAMalformedResource(t *testing.T) {
	for _, bad := range []string{"dossier", "dossier:a:b", "dossier:a/b", ""} {
		res, err := meta.NewRecordObservation().Execute(context.Background(),
			json.RawMessage(`{"resource":"`+bad+`","text":"x"}`), sessionWithPoolWriter(nil))
		require.NoError(t, err)
		assert.True(t, res.IsError, "resource %q", bad)
	}
}

func TestRecordObservationRefusesEmptyText(t *testing.T) {
	res, err := meta.NewRecordObservation().Execute(context.Background(),
		json.RawMessage(`{"resource":"dossier:d-1","text":""}`), sessionWithPoolWriter(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

// A server-side refusal (no write grant) must reach the model as a readable
// refusal, not a panic or a bare error string.
func TestRecordObservationSurfacesAServerRefusal(t *testing.T) {
	sess := sessionWithPoolWriter(func(context.Context, memory.Scope, memory.Scope, memory.Entry) (memory.Entry, error) {
		return memory.Entry{}, fmt.Errorf("memory: this session holds no write grant on ledger:l-9")
	})
	res, err := meta.NewRecordObservation().Execute(context.Background(),
		json.RawMessage(`{"resource":"ledger:l-9","text":"x"}`), sess)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "no write grant")
}

// TestRecordObservationRefusesWithNoMemory pins the "memory not available"
// branch: a nil sess.Mem must not panic, and the refusal must be Trusted
// framework text — mirrors query_memory's own no-memory regression test.
func TestRecordObservationRefusesWithNoMemory(t *testing.T) {
	res, err := meta.NewRecordObservation().Execute(context.Background(),
		json.RawMessage(`{"resource":"dossier:d-1","text":"x"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.True(t, res.Trusted)
}

// TestRecordObservationRefusesWhenMemDoesNotSupportPoolWrites pins the
// capability-assertion branch: a sess.Mem that answers query_memory's needs
// but has no PutToPool (e.g. the in-process facade wired for query-only use)
// must refuse readably rather than panic on a failed type assertion.
func TestRecordObservationRefusesWhenMemDoesNotSupportPoolWrites(t *testing.T) {
	sess := &tool.SessionContext{Namespace: "default", Name: "s1", Mem: queryOnlyMem{}}
	res, err := meta.NewRecordObservation().Execute(context.Background(),
		json.RawMessage(`{"resource":"dossier:d-1","text":"x"}`), sess)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.True(t, res.Trusted)
}

// queryOnlyMem satisfies tool.MemoryQuerier and nothing else — no PutToPool.
type queryOnlyMem struct{}

func (queryOnlyMem) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}

func (queryOnlyMem) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}
