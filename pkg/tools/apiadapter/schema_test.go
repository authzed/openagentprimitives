package apiadapter_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/apiadapter"
)

func TestInputSchema_DerivesPropertiesAndRequired(t *testing.T) {
	c, err := apiadapter.Parse([]byte(goodConfig))
	require.NoError(t, err)
	op, ok := c.Operation("get_account")
	require.True(t, ok)

	s := op.InputSchema()
	assert.Equal(t, "object", s["type"])
	props := s["properties"].(map[string]any)
	assert.Equal(t, map[string]any{"type": "string", "description": "Account id."}, props["id"])
	assert.Equal(t, map[string]any{"type": "boolean", "description": "Include detail."}, props["verbose"])
	assert.Equal(t, []any{"id"}, s["required"], "only the required param is listed")
}

func TestInputSchema_NoRequiredKeyWhenNonePresent(t *testing.T) {
	c, err := apiadapter.Parse([]byte("baseURL: https://a.test\nauth: {type: none}\noperations: [{name: a, method: GET, path: /x, params: [{name: q, in: query, type: string}]}]"))
	require.NoError(t, err)
	op, _ := c.Operation("a")
	_, has := op.InputSchema()["required"]
	assert.False(t, has, "an all-optional operation omits required entirely")
}
