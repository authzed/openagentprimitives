package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mempostgres "github.com/authzed/openagentprimitives/pkg/memory/postgres"
)

func TestOpenPostgresWithRetry_FirstTrySuccessReturnsClient(t *testing.T) {
	want := &mempostgres.Client{}
	calls := 0
	open := func(context.Context) (*mempostgres.Client, error) { calls++; return want, nil }

	got, err := openPostgresWithRetry(context.Background(), logr.Discard(), time.Second, open)
	require.NoError(t, err)
	assert.Same(t, want, got)
	assert.Equal(t, 1, calls)
}

func TestOpenPostgresWithRetry_CeilingExceededReturnsLoudError(t *testing.T) {
	open := func(context.Context) (*mempostgres.Client, error) {
		return nil, errors.New("dial tcp 10.0.0.1:5432: connect: connection refused")
	}
	got, err := openPostgresWithRetry(context.Background(), logr.Discard(), 10*time.Millisecond, open)
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "postgres")
}
