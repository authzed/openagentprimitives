package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func TestSanitizeToolOutput(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		check func(t *testing.T, out string)
	}{
		{
			name: "text with no NUL: returned unchanged",
			in:   "diff --git a/x b/x\n+hello\n",
			check: func(t *testing.T, out string) {
				assert.Equal(t, "diff --git a/x b/x\n+hello\n", out)
			},
		},
		{
			name: "NUL at offset 0 (an ELF header): replaced with a sized binary notice",
			in:   "\x7fELF\x02\x01\x01\x00" + strings.Repeat("x", 2040),
			check: func(t *testing.T, out string) {
				assert.Contains(t, out, "[binary output omitted: 2.0 kB.")
				assert.NotContains(t, out, "ELF", "none of the binary bytes may survive")
			},
		},
		{
			name: "NUL just inside the sniff window: treated as binary",
			in:   strings.Repeat("a", binarySniffLen-1) + "\x00",
			check: func(t *testing.T, out string) {
				assert.True(t, strings.HasPrefix(out, "[binary output omitted:"), "got %q…", out[:min(len(out), 40)])
			},
		},
		{
			name: "NUL past the sniff window: only the NUL is replaced with U+FFFD",
			in:   strings.Repeat("a", binarySniffLen) + "\x00tail",
			check: func(t *testing.T, out string) {
				assert.Equal(t, strings.Repeat("a", binarySniffLen)+"�tail", out)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := sanitizeToolOutput(tc.in)
			assert.NotContains(t, out, "\x00", "sanitized output must never carry NUL")
			tc.check(t, out)
		})
	}
}

// TestDispatch_BinaryToolOutputIsSanitizedOnBothPaths pins the regression where
// a `git show <sha>:<binary>` result carried NUL into the tool_result turn,
// Postgres jsonb refused it (SQLSTATE 22P05), and the session failed as
// MemoryUnavailable. executeToolRaw is the one place both dispatch paths
// execute through, so both must come back clean.
func TestDispatch_BinaryToolOutputIsSanitizedOnBothPaths(t *testing.T) {
	binary := "\x7fELF\x02\x01\x01\x00\x00\x00binary-body"
	cases := []struct {
		name string
		kind tool.Kind
		perm authz.Permission
	}{
		{name: "gated MCP tool: result sanitized", kind: tool.KindMCP},
		{name: "ungated meta tool: result sanitized", kind: tool.KindMeta, perm: authz.Permission{StateImpact: authz.Stateless}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ft := &fakeDispatchTool{name: "git", kind: tc.kind, perm: tc.perm, result: tool.Result{Content: binary}}
			l := &Loop{
				Tools:      []tool.Tool{ft},
				SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"},
			}
			loopWithInjectedExecutor(t, l, pipeline.NewRegistry())

			uses := []llm.ToolUseBlock{{ID: "tu-1", Name: "git", Input: json.RawMessage(`{"args":{}}`)}}
			results, _ := l.dispatchToolUses(memory.WithSystemApproval(context.Background(), "test"), uses, dispatchTestSession(), 0, 0, nil, nil)

			require.Len(t, results, 1)
			assert.Equal(t, 1, ft.executed())
			assert.NotContains(t, results[0].Content, "\x00")
			assert.Contains(t, results[0].Content, "[binary output omitted:")
		})
	}
}
