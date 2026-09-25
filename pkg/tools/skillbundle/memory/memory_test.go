package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
)

func TestPutGetHas(t *testing.T) {
	ctx := context.Background()
	s := New()

	has, err := s.Has(ctx, "deadbeef")
	require.NoError(t, err)
	assert.False(t, has)

	require.NoError(t, s.Put(ctx, "deadbeef", []byte("payload")))
	has, err = s.Has(ctx, "deadbeef")
	require.NoError(t, err)
	assert.True(t, has)

	got, err := s.Get(ctx, "deadbeef")
	require.NoError(t, err)
	assert.Equal(t, []byte("payload"), got)

	_, err = s.Get(ctx, "missing")
	assert.ErrorIs(t, err, skillbundle.ErrNotFound)
}
