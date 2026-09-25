package memory_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestFieldOp_ZeroValueIsEq(t *testing.T) {
	f := memory.FieldFilter{Path: "valid_at", Value: "2024-01-01"}
	assert.Equal(t, memory.FieldOp(""), f.Op, "zero value Op should be empty string")
	assert.Equal(t, memory.FieldOpEq, memory.NormalizeFieldOp(f.Op),
		"NormalizeFieldOp should treat empty as Eq")
}

func TestFieldFilter_JSONRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		filter memory.FieldFilter
	}{
		{
			name:   "eq with value",
			filter: memory.FieldFilter{Path: "status", Op: memory.FieldOpEq, Value: "active"},
		},
		{
			name:   "lte with timestamp",
			filter: memory.FieldFilter{Path: "valid_at", Op: memory.FieldOpLte, Value: "2024-03-15T00:00:00Z"},
		},
		{
			name:   "is_nil no value",
			filter: memory.FieldFilter{Path: "invalid_at", Op: memory.FieldOpIsNil},
		},
		{
			name:   "zero op omitted in JSON",
			filter: memory.FieldFilter{Path: "x", Value: 42},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.filter)
			require.NoError(t, err)

			var got memory.FieldFilter
			require.NoError(t, json.Unmarshal(b, &got))
			assert.Equal(t, tc.filter.Path, got.Path)
			assert.Equal(t, tc.filter.Op, got.Op)
		})
	}
}
