package scope_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

func TestColdStartExtraction_RoundTripJSON(t *testing.T) {
	c := scope.ColdStartExtraction{
		ScopeDelta: scope.ScopeDelta{
			HardDeny: scope.ScopePartial{
				ResourcePatterns: []scope.ResourcePattern{{Attrs: map[string]string{"team": "ENG"}}},
			},
		},
		CleanedTask: "summarize linear issue L-140",
	}
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	var back scope.ColdStartExtraction
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, c, back)
}

func TestColdStartExtraction_EmptyCleanedTask(t *testing.T) {
	c := scope.ColdStartExtraction{ScopeDelta: scope.ScopeDelta{}}
	assert.Empty(t, c.CleanedTask)
	assert.True(t, c.ScopeDelta.IsEmpty())
}
