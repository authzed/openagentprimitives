package anthropic_test

import (
	"testing"

	_ "github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic" // registers "anthropic"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/providers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnthropicRegistered(t *testing.T) {
	p, err := providers.New("anthropic", "sk-test")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "anthropic", p.Name())

	// Empty provider name defaults to anthropic.
	p2, err := providers.New("", "sk-test")
	require.NoError(t, err)
	assert.Equal(t, "anthropic", p2.Name())

	// Empty key is an error through the factory (not a panic).
	_, err = providers.New("anthropic", "")
	require.Error(t, err)
}
