package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
	skillbundlepg "github.com/authzed/openagentprimitives/pkg/tools/skillbundle/postgres"
)

func newTestStore(t *testing.T) *skillbundlepg.Store {
	t.Helper()
	uri := os.Getenv("POSTGRES_URI")
	if uri == "" {
		t.Skip("POSTGRES_URI not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, uri)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	store := skillbundlepg.New(pool)
	require.NoError(t, store.Migrate(ctx))

	// Clean the table between tests for isolation.
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM skill_bundle") //nolint:errcheck
	})
	return store
}

func TestPostgresStore_PutGetHas(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const digest = "sha256:deadbeef"
	payload := []byte("bundle contents")

	// Not present initially.
	has, err := s.Has(ctx, digest)
	require.NoError(t, err)
	assert.False(t, has, "digest should not be present before Put")

	// Get on missing digest returns ErrNotFound.
	_, err = s.Get(ctx, digest)
	assert.ErrorIs(t, err, skillbundle.ErrNotFound, "Get on missing digest should return ErrNotFound")

	// Put stores the bundle.
	require.NoError(t, s.Put(ctx, digest, payload))

	// Has now true.
	has, err = s.Has(ctx, digest)
	require.NoError(t, err)
	assert.True(t, has, "digest should be present after Put")

	// Get returns the correct bytes.
	got, err := s.Get(ctx, digest)
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	// Second Put for same digest is a no-op (ON CONFLICT DO NOTHING).
	require.NoError(t, s.Put(ctx, digest, []byte("different")), "idempotent second Put should not error")
	got2, err := s.Get(ctx, digest)
	require.NoError(t, err)
	assert.Equal(t, payload, got2, "second Put should not overwrite existing data")
}
