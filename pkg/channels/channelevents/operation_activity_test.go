package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKindOperationActivity_Implemented(t *testing.T) {
	assert.True(t, KindOperationActivity.Implemented(), "new kind must be routable")
}

func TestOperationActivityPayload_JSONRoundTrip(t *testing.T) {
	in := OperationActivityPayload{
		Operations: []OperationActivityNode{{
			ID: "op-1", Description: "fetch leadership goals", ElapsedSeconds: 12, Active: true,
			Calls: []OperationActivityCallNode{{Tool: "linear_search", Reason: "querying Q3 goals", ElapsedSeconds: 4, Active: true}},
		}},
		CompactLine: "fetch leadership goals ‣ querying Q3 goals",
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	var out OperationActivityPayload
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}

func TestOperationActivityCompactLine(t *testing.T) {
	cases := []struct {
		name string
		ops  []OperationActivityNode
		want string
	}{
		{name: "op with active call: op ‣ reason",
			ops:  []OperationActivityNode{{Description: "fetch goals", Active: true, Calls: []OperationActivityCallNode{{Reason: "querying Linear", Active: true}}}},
			want: "fetch goals ‣ querying Linear"},
		{name: "active op, no active call: op only",
			ops:  []OperationActivityNode{{Description: "fetch goals", Active: true}},
			want: "fetch goals"},
		{name: "deepest active leaf wins across two ops",
			ops: []OperationActivityNode{
				{Description: "outer", Active: true, Calls: []OperationActivityCallNode{{Reason: "old", Active: false}}},
				{Description: "inner", Active: true, Calls: []OperationActivityCallNode{{Reason: "querying now", Active: true}}}},
			want: "inner ‣ querying now"},
		{name: "no active op: empty", ops: []OperationActivityNode{{Description: "done", Active: false}}, want: ""},
		{name: "nil: empty", ops: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, OperationActivityCompactLine(tc.ops)) })
	}
}
