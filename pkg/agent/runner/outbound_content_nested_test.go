package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

func nestedArgsLoop() *Loop {
	return &Loop{
		Tools: []tool.Tool{fakeDestTool{
			name: "pde_post_to_board",
			perm: authz.Permission{
				StateImpact: authz.Readwrite,
				Check: &authz.PermissionCheck{
					ResourceType:   "pde_board",
					Permission:     "post",
					ResourceIDExpr: "string(args.board_id)",
				},
			},
		}},
	}
}

// outboundContent is what the coverage check measures, and it only ever
// appended top-level string args — any array or object value was skipped in
// silence.
//
// That is the whole gap: one tagged top-level string makes the payload look
// covered, so the untagged datum rides out nested inside blocks[].text under
// the top-level tags' audience. The all-nested case was already safe (no
// parts, so coverage fails and the call drops to the floor), which is exactly
// why the partially-nested case is the one that slips through.
//
// The function's own contract says "every remaining string must be tagged or
// coverage fails", so this is a silent under-delivery against its stated rule.
func TestOutboundContent_IncludesNestedStrings(t *testing.T) {
	l := nestedArgsLoop()

	raw, err := json.Marshal(map[string]any{
		"operation_id": "op-1",
		"_reason":      "posting",
		"args": map[string]any{
			"board_id": "board-team",
			"summary":  "tagged-top-level",
			"blocks": []any{
				map[string]any{"type": "section", "text": "NESTED-SECRET"},
			},
			"meta": map[string]any{"description": "NESTED-IN-OBJECT"},
		},
	})
	require.NoError(t, err)

	got := l.outboundContent(context.Background(), "pde_post_to_board", raw)

	assert.Contains(t, got, "tagged-top-level", "the top-level string must still be measured")
	assert.Contains(t, got, "NESTED-SECRET",
		"a string inside an array of objects carries data out and must be measured")
	assert.Contains(t, got, "NESTED-IN-OBJECT",
		"a string inside a nested object carries data out and must be measured")
}

// The routing arg the tool's own Check consumes is excluded so it may stay
// untagged — that exclusion must remain top-level-only, or a nested field
// sharing a routing arg's name would silently stop being measured.
func TestOutboundContent_ExcludesRoutingArgOnlyAtTopLevel(t *testing.T) {
	l := nestedArgsLoop()

	raw, err := json.Marshal(map[string]any{
		"operation_id": "op-1",
		"_reason":      "posting",
		"args": map[string]any{
			"board_id": "board-team",
			"payload":  map[string]any{"board_id": "NESTED-SAME-NAME"},
		},
	})
	require.NoError(t, err)

	got := l.outboundContent(context.Background(), "pde_post_to_board", raw)

	assert.NotContains(t, got, "board-team", "the top-level routing arg stays excluded")
	assert.Contains(t, got, "NESTED-SAME-NAME",
		"a nested field is content regardless of its name; only the Check's own top-level arg is routing")
}

// Deeply nested strings must still be reached — an attacker choosing the
// nesting depth must not choose whether the gate looks.
func TestOutboundContent_ReachesDeeplyNestedStrings(t *testing.T) {
	l := nestedArgsLoop()

	raw, err := json.Marshal(map[string]any{
		"operation_id": "op-1",
		"_reason":      "posting",
		"args": map[string]any{
			"board_id": "board-team",
			"a":        map[string]any{"b": []any{map[string]any{"c": "DEEP-SECRET"}}},
		},
	})
	require.NoError(t, err)

	assert.Contains(t, l.outboundContent(context.Background(), "pde_post_to_board", raw), "DEEP-SECRET")
}
