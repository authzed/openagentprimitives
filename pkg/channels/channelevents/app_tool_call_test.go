package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppToolCallKind_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindAppToolCall.Valid(), "KindAppToolCall must be Valid")
	assert.True(t, KindAppToolCall.Implemented(), "KindAppToolCall must be Implemented")
}

func TestAppToolCallRequest_RoundTrip(t *testing.T) {
	want := AppToolCallRequest{
		ToolName:  "widgettool_list_items",
		Args:      json.RawMessage(`{"page":2}`),
		Requester: "user:demo-viewer",
		RequestID: "req-1",
	}

	raw, err := json.Marshal(want)
	require.NoError(t, err)

	var got AppToolCallRequest
	require.NoError(t, json.Unmarshal(raw, &got))

	assert.Equal(t, want.ToolName, got.ToolName)
	assert.JSONEq(t, string(want.Args), string(got.Args))
	assert.Equal(t, want.Requester, got.Requester)
	assert.Equal(t, want.RequestID, got.RequestID)
}

func TestAppToolCallResponse_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		want AppToolCallResponse
	}{
		{
			name: "ok with result",
			want: AppToolCallResponse{
				Status: AppToolCallStatusOK,
				Result: json.RawMessage(`{"items":["a","b"]}`),
			},
		},
		{
			name: "ok tool-level error",
			want: AppToolCallResponse{
				Status:  AppToolCallStatusOK,
				Result:  json.RawMessage(`{"reason":"not found"}`),
				IsError: true,
			},
		},
		{
			name: "requires approval",
			want: AppToolCallResponse{
				Status:  AppToolCallStatusRequiresApproval,
				Message: "widgettool_delete_item is not readonly",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.want)
			require.NoError(t, err)

			var got AppToolCallResponse
			require.NoError(t, json.Unmarshal(raw, &got))

			assert.Equal(t, tc.want.Status, got.Status)
			assert.Equal(t, tc.want.IsError, got.IsError)
			assert.Equal(t, tc.want.Message, got.Message)
			if tc.want.Result == nil {
				assert.Nil(t, got.Result)
			} else {
				assert.JSONEq(t, string(tc.want.Result), string(got.Result))
			}
		})
	}
}
