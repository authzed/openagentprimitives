package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestUnwrapToolArgs covers the conditional unwrap used by the per-tool
// authz pipeline. Every tool that goes through tool.WrapInputSchema
// (every MCP tool and every sandbox tool) emits its args wrapped in
// {operation_id, _reason, args}. Authz CEL expressions in MCPServer
// CRs are written against the INNER args (e.g. `args.objectType ==
// "contacts"`), so the runner must unwrap before evaluating variant
// When clauses and resourceIDExpr.
//
// Meta tools (respond_to_user, lookup_user_for_mention, …) have flat
// input schemas — no wrapping — so the unwrap must be a no-op for
// them. A flat tool input that happens to carry an `args` key (for
// whatever reason) is NOT a wrapped envelope unless an operation_id
// also rides along; we use that pair as the canary so meta-tool
// callers stay unaffected.
func TestUnwrapToolArgs(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{
			name: "wrapped envelope: returns the inner args map",
			in: map[string]any{
				"operation_id": "op-1",
				"_reason":      "fetch contacts",
				"args": map[string]any{
					"objectType":   "contacts",
					"filterGroups": []any{},
				},
			},
			want: map[string]any{
				"objectType":   "contacts",
				"filterGroups": []any{},
			},
		},
		{
			name: "meta-tool flat shape: no operation_id, unchanged",
			in: map[string]any{
				"kind":  "email",
				"value": "fred@example.com",
			},
			want: map[string]any{
				"kind":  "email",
				"value": "fred@example.com",
			},
		},
		{
			name: "no args field but has operation_id: unchanged (defensive)",
			in: map[string]any{
				"operation_id": "op-1",
				"_reason":      "",
			},
			want: map[string]any{
				"operation_id": "op-1",
				"_reason":      "",
			},
		},
		{
			name: "args is not a map (corrupt): unchanged",
			in: map[string]any{
				"operation_id": "op-1",
				"_reason":      "",
				"args":         "should be a map",
			},
			want: map[string]any{
				"operation_id": "op-1",
				"_reason":      "",
				"args":         "should be a map",
			},
		},
		{
			name: "nil input: returns nil unchanged",
			in:   nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unwrapToolArgs(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}
