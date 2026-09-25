package memory_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestKGScopeRoundTrip(t *testing.T) {
	t.Run("unset context returns ok=false", func(t *testing.T) {
		_, ok := memory.KGScopeFrom(context.Background())
		assert.False(t, ok)
	})

	t.Run("scope with non-empty ID round-trips with ok=true", func(t *testing.T) {
		want := memory.Scope{Kind: "session", ID: "nsA/sessA"}
		ctx := memory.WithKGScope(context.Background(), want)
		got, ok := memory.KGScopeFrom(ctx)
		assert.True(t, ok)
		assert.Equal(t, want, got)
	})

	t.Run("scope with empty ID treated as unset", func(t *testing.T) {
		ctx := memory.WithKGScope(context.Background(), memory.Scope{})
		_, ok := memory.KGScopeFrom(ctx)
		assert.False(t, ok)
	})
}
