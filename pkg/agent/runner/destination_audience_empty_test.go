package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// egressLoop builds a Loop whose one tool is a readwrite egress tool with a
// resolvable destination, and whose subject lookup returns whatever the test
// supplies. Only the lookup's answer varies between the cases below.
func egressLoop(lookup func(ctx context.Context, resource, permission string) ([]string, error)) *Loop {
	return &Loop{
		AgentClass:            classGranting(map[string]string{"fine_grained_info_leakage": `{}`}),
		SpiceDBLookupSubjects: lookup,
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
		LookupToolMapping: func(name string) *spiceboxv1alpha1.ToolResourceMapping {
			if name != "pde_post_to_board" {
				return nil
			}
			return &spiceboxv1alpha1.ToolResourceMapping{
				Tool:  "post_to_board",
				Reads: &spiceboxv1alpha1.ToolReads{ResourceType: "pde_board", IDArg: "board_id", Permission: "view"},
			}
		},
	}
}

// boardArgs is the MCP envelope shape the model actually sends, which
// destinationAudience unwraps before evaluating the Check's expr.
func boardArgs(id string) map[string]any {
	return map[string]any{
		"operation_id": "op-1",
		"_reason":      "posting",
		"args":         map[string]any{"board_id": id, "text": "secret"},
	}
}

// SpiceDB returns an empty subject list for two very different situations: a
// destination that genuinely has no readers, and an object id that is not
// modelled at all — LookupSubjects does not error on an unknown object.
//
// The hook's own contract says a destination that cannot be resolved has an
// UNKNOWN audience, "which is not an empty one", and that returning one for
// "I don't know" would read as permission to send anywhere. It could not
// honour that: it returned (nil, true), and downstream an empty audience is
// unauthorized-by-nobody, so both floors ALLOW.
//
// An egress destination with no readers is not a destination worth trusting
// either way, so the fix is the same for both readings: report it unresolved
// and let the Mode decide.
func TestDestinationAudience_EmptyLookupIsUnresolvedNotEmpty(t *testing.T) {
	l := egressLoop(func(context.Context, string, string) ([]string, error) {
		return nil, nil
	})

	audience, resolved, err := l.destinationAudience(context.Background(), "pde_post_to_board", boardArgs("no-such-board"))
	require.NoError(t, err)
	assert.False(t, resolved,
		"an empty subject set must read as UNKNOWN, not as an audience of nobody that every check vacuously passes")
	assert.Empty(t, audience)
}

// A destination that does resolve to readers is unaffected: this must not turn
// the ordinary egress path into a denial.
func TestDestinationAudience_NonEmptyLookupStaysResolved(t *testing.T) {
	l := egressLoop(func(context.Context, string, string) ([]string, error) {
		return []string{"user:you", "user:teammate"}, nil
	})

	audience, resolved, err := l.destinationAudience(context.Background(), "pde_post_to_board", boardArgs("board-team"))
	require.NoError(t, err)
	assert.True(t, resolved)
	assert.Equal(t, []string{"user:you", "user:teammate"}, audience)
}
