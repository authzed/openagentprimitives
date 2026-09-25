package memory_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestCallerFrom_AbsentReturnsEmpty(t *testing.T) {
	caller, ok := memory.CallerFrom(context.Background())
	assert.False(t, ok)
	assert.Equal(t, "", caller)
}

func TestCallerFrom_RoundTrip(t *testing.T) {
	ctx := memory.WithCaller(context.Background(), "user-abc")
	caller, ok := memory.CallerFrom(ctx)
	assert.True(t, ok)
	assert.Equal(t, "user-abc", caller)
}

func TestCallerFrom_EmptyStringIsAbsent(t *testing.T) {
	ctx := memory.WithCaller(context.Background(), "")
	_, ok := memory.CallerFrom(ctx)
	assert.False(t, ok, "empty string should be treated as absent")
}
