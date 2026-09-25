package shadow_test

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/shadow"
)

func testLogger(t *testing.T) (logr.Logger, *[]string) {
	t.Helper()
	var msgs []string
	l := funcr.New(func(prefix, args string) {
		msgs = append(msgs, prefix+" "+args)
	}, funcr.Options{})
	return l, &msgs
}

func TestShadow_WritesFanOutToBoth(t *testing.T) {
	primary := inmem.NewBackend()
	secondary := inmem.NewBackend()
	logger, _ := testLogger(t)
	b, err := shadow.New(primary, secondary, shadow.ReadFromPrimary, logger)
	require.NoError(t, err)

	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	e := memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC()}

	require.NoError(t, b.Put(ctx, e))

	got1, ok1, err1 := primary.Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err1)
	require.True(t, ok1)
	assert.Equal(t, "a-1", got1.ID)

	got2, ok2, err2 := secondary.Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err2)
	require.True(t, ok2)
	assert.Equal(t, "a-1", got2.ID)
}

func TestShadow_DeleteFansOutToBoth(t *testing.T) {
	primary := inmem.NewBackend()
	secondary := inmem.NewBackend()
	logger, _ := testLogger(t)
	b, err := shadow.New(primary, secondary, shadow.ReadFromPrimary, logger)
	require.NoError(t, err)

	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	e := memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC()}
	require.NoError(t, b.Put(ctx, e))
	require.NoError(t, b.Delete(ctx, scope, "alpha", "a-1"))

	_, ok1, _ := primary.Get(ctx, scope, "alpha", "a-1")
	assert.False(t, ok1)
	_, ok2, _ := secondary.Get(ctx, scope, "alpha", "a-1")
	assert.False(t, ok2)
}

func TestShadow_DeleteScopeFansOutToBoth(t *testing.T) {
	primary := inmem.NewBackend()
	secondary := inmem.NewBackend()
	logger, _ := testLogger(t)
	b, err := shadow.New(primary, secondary, shadow.ReadFromPrimary, logger)
	require.NoError(t, err)

	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	require.NoError(t, b.Put(ctx, memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC()}))
	require.NoError(t, b.DeleteScope(ctx, scope))

	res1, _ := primary.Query(ctx, memory.Query{Scope: scope})
	assert.Empty(t, res1.Entries)
	res2, _ := secondary.Query(ctx, memory.Query{Scope: scope})
	assert.Empty(t, res2.Entries)
}

func TestShadow_ReadsFromPrimary(t *testing.T) {
	primary := inmem.NewBackend()
	secondary := inmem.NewBackend()
	logger, _ := testLogger(t)
	b, err := shadow.New(primary, secondary, shadow.ReadFromPrimary, logger)
	require.NoError(t, err)

	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	// Put directly into primary only (bypass shadow).
	require.NoError(t, primary.Put(ctx, memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC()}))

	got, ok, err := b.Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err)
	require.True(t, ok, "should read from primary")
	assert.Equal(t, "a-1", got.ID)

	// Entry not in secondary, so reading from secondary would miss it.
	_, ok2, _ := secondary.Get(ctx, scope, "alpha", "a-1")
	assert.False(t, ok2)
}

func TestShadow_ReadsFromSecondary(t *testing.T) {
	primary := inmem.NewBackend()
	secondary := inmem.NewBackend()
	logger, _ := testLogger(t)
	b, err := shadow.New(primary, secondary, shadow.ReadFromSecondary, logger)
	require.NoError(t, err)

	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	// Put directly into secondary only (bypass shadow).
	require.NoError(t, secondary.Put(ctx, memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC()}))

	got, ok, err := b.Get(ctx, scope, "alpha", "a-1")
	require.NoError(t, err)
	require.True(t, ok, "should read from secondary")
	assert.Equal(t, "a-1", got.ID)

	// Not in primary.
	_, ok2, _ := primary.Get(ctx, scope, "alpha", "a-1")
	assert.False(t, ok2)
}

func TestShadow_CapabilitiesFromReadSource(t *testing.T) {
	primary := inmem.NewBackend()
	secondary := inmem.NewBackend()
	logger, _ := testLogger(t)

	bp, err := shadow.New(primary, secondary, shadow.ReadFromPrimary, logger)
	require.NoError(t, err)
	bs, err := shadow.New(primary, secondary, shadow.ReadFromSecondary, logger)
	require.NoError(t, err)

	// Both are inmem so capabilities are the same, but the test
	// verifies delegation: capabilities come from the readFrom backend.
	assert.Equal(t, primary.Capabilities(), bp.Capabilities())
	assert.Equal(t, secondary.Capabilities(), bs.Capabilities())
}

// TestShadow_DurableSecondaryWriteErrorIsReturned pins the durability
// contract: the secondary is the authoritative durable half (the only
// construction site wires primary=inmem, secondary=postgres), so a failed
// secondary write MUST reach the caller — under BOTH read sources, because
// readFrom selects where reads are served from, not which half is durable.
//
// The ReadFromSecondary rows are the shipped configuration: the cluster
// bundle sets MEMORY_BACKEND=postgres and leaves MEMORY_READ_SOURCE unset,
// which the operator defaults to "secondary". Swallowing the error there
// tells the caller a write landed while every subsequent read goes to the
// store that never got it.
func TestShadow_DurableSecondaryWriteErrorIsReturned(t *testing.T) {
	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	writes := map[string]struct {
		call    func(b *shadow.Backend) error
		logWant string
	}{
		"Put": {
			call: func(b *shadow.Backend) error {
				return b.Put(ctx, memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC()})
			},
			logWant: "secondary Put",
		},
		"Delete": {
			call:    func(b *shadow.Backend) error { return b.Delete(ctx, scope, "alpha", "a-1") },
			logWant: "secondary Delete",
		},
		"DeleteScope": {
			call:    func(b *shadow.Backend) error { return b.DeleteScope(ctx, scope) },
			logWant: "secondary DeleteScope",
		},
	}

	for _, readFrom := range []string{shadow.ReadFromPrimary, shadow.ReadFromSecondary} {
		for op, w := range writes {
			t.Run(op+" with failing secondary, readFrom="+readFrom+": error returned to caller and logged", func(t *testing.T) {
				logger, msgs := testLogger(t)
				b, err := shadow.New(inmem.NewBackend(), &failBackend{}, readFrom, logger)
				require.NoError(t, err)

				err = w.call(b)

				require.Error(t, err, "durable secondary write failed; caller must NOT be told it succeeded")
				assert.ErrorIs(t, err, assert.AnError, "the secondary's error must be wrapped, not replaced")
				require.NotEmpty(t, *msgs, "the primary/secondary divergence should also be logged")
				assert.Contains(t, (*msgs)[0], w.logWant)
			})
		}
	}
}

func TestShadow_SecondaryWriteErrorStillReachesPrimary(t *testing.T) {
	primary := inmem.NewBackend()
	logger, _ := testLogger(t)
	b, err := shadow.New(primary, &failBackend{}, shadow.ReadFromSecondary, logger)
	require.NoError(t, err)

	ctx := context.Background()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	e := memory.Entry{Scope: scope, Kind: "alpha", ID: "a-1", CreatedAt: time.Now().UTC()}

	// The write is still fanned out — the primary half is not rolled back —
	// but the caller is told the durable half rejected it.
	require.Error(t, b.Put(ctx, e))

	_, ok, _ := primary.Get(ctx, scope, "alpha", "a-1")
	assert.True(t, ok, "primary keeps what it accepted; the caller retries against both")
}

func TestShadow_InvalidReadFromReturnsError(t *testing.T) {
	primary := inmem.NewBackend()
	secondary := inmem.NewBackend()
	logger, _ := testLogger(t)
	_, err := shadow.New(primary, secondary, "bogus", logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid readFrom")
}

// failBackend is a Backend that always returns errors on writes.
type failBackend struct{}

func (failBackend) Capabilities() memory.Capabilities { return memory.Capabilities{} }
func (failBackend) Put(context.Context, memory.Entry) error {
	return assert.AnError
}
func (failBackend) Get(context.Context, memory.Scope, string, string) (memory.Entry, bool, error) {
	return memory.Entry{}, false, nil
}
func (failBackend) Delete(context.Context, memory.Scope, string, string) error {
	return assert.AnError
}
func (failBackend) DeleteScope(context.Context, memory.Scope) error {
	return assert.AnError
}
func (failBackend) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}
func (failBackend) QueryAllScopes(context.Context, memory.CrossScopeQuery) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}
func (failBackend) Status(context.Context, memory.Scope) (memory.ScopeStatus, error) {
	return memory.ScopeStatus{}, nil
}
