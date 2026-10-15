package bootstrap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingSchema struct {
	calls int
	last  string
}

func (r *recordingSchema) WriteSchema(_ context.Context, text string) error {
	r.calls++
	r.last = text
	return nil
}

func TestRunBootstrap_AppliesSchemaIdempotently(t *testing.T) {
	sch := &recordingSchema{}
	require.NoError(t, RunBootstrap(context.Background(), sch, "definition user {}"))
	require.NoError(t, RunBootstrap(context.Background(), sch, "definition user {}"))
	assert.Equal(t, 2, sch.calls, "WriteSchema runs each invocation (it is idempotent)")
	assert.Equal(t, "definition user {}", sch.last)
}
