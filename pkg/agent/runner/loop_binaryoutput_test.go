package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/secretout"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func TestSanitizeToolOutput(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		cleanup outputCleanup
		check   func(t *testing.T, out string)
	}{
		{
			name:    "text with no NUL: returned unchanged",
			in:      "diff --git a/x b/x\n+hello\n",
			cleanup: outputUnchanged,
			check: func(t *testing.T, out string) {
				assert.Equal(t, "diff --git a/x b/x\n+hello\n", out)
			},
		},
		{
			name:    "an ELF header (NUL plus control bytes): replaced with a sized binary notice",
			in:      "\x7fELF\x02\x01\x01\x00" + strings.Repeat("x", 2040),
			cleanup: outputBinaryOmitted,
			check: func(t *testing.T, out string) {
				assert.Contains(t, out, "[binary output omitted: 2.0 kB.")
				assert.NotContains(t, out, "ELF", "none of the binary bytes may survive")
			},
		},
		{
			name:    "a PNG header (NUL plus invalid UTF-8): replaced with a binary notice",
			in:      "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR",
			cleanup: outputBinaryOmitted,
			check: func(t *testing.T, out string) {
				assert.True(t, strings.HasPrefix(out, "[binary output omitted:"), "got %q", out)
			},
		},
		{
			name:    "NUL-separated listing (git ls-files -z): every name kept, NULs replaced with U+FFFD",
			in:      "a.go\x00b.go\x00c.go\x00",
			cleanup: outputNULReplaced,
			check: func(t *testing.T, out string) {
				assert.Equal(t, "a.go\ufffdb.go\ufffdc.go\ufffd", out)
			},
		},
		{
			name:    "NUL-separated listing with non-ASCII names and terminal colour: still text",
			in:      "\x1b[32mnaïve.go\x1b[0m\x00日本語.txt\x00",
			cleanup: outputNULReplaced,
			check: func(t *testing.T, out string) {
				assert.Contains(t, out, "naïve.go")
				assert.Contains(t, out, "日本語.txt")
			},
		},
		{
			name:    "a multi-byte character straddling the sniff window's edge: not mistaken for invalid UTF-8",
			in:      "a\x00" + strings.Repeat("b", binarySniffLen-3) + "日tail",
			cleanup: outputNULReplaced,
			check: func(t *testing.T, out string) {
				assert.True(t, strings.HasSuffix(out, "日tail"), "the text must be kept")
			},
		},
		{
			name:    "binary bytes only past the sniff window, a stray NUL before them: text kept, NULs replaced",
			in:      strings.Repeat("a", binarySniffLen) + "\x00\x01tail",
			cleanup: outputNULReplaced,
			check: func(t *testing.T, out string) {
				assert.Equal(t, strings.Repeat("a", binarySniffLen)+"\ufffd\x01tail", out)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, cleanup := sanitizeToolOutput(tc.in)
			assert.Equal(t, tc.cleanup, cleanup)
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
			results := dispatchOnce(t, tc.kind, tc.perm, tool.Result{Content: binary})

			require.Len(t, results, 1)
			assert.NotContains(t, results[0].Content, "\x00")
			assert.Contains(t, results[0].Content, "[binary output omitted:")
		})
	}
}

// TestDispatch_OutputSanitizerExceptions pins the two results the sanitizer
// must not treat like ordinary output, on the same dispatch path as above.
func TestDispatch_OutputSanitizerExceptions(t *testing.T) {
	cases := []struct {
		name   string
		result tool.Result
		check  func(t *testing.T, got tool.Result)
	}{
		{
			// applySecretOutput stores Content as the credential. A sanitized
			// value would be captured, reported as success, and be wrong.
			name: "secret-output result that looks binary: Content reaches the capture byte-for-byte",
			result: tool.Result{
				Content:      "\x00\x01\x02key\xff",
				SecretOutput: &secretout.Spec{Name: "token"},
			},
			check: func(t *testing.T, got tool.Result) {
				require.NotNil(t, got.SecretOutput, "the result must still be marked for capture")
				assert.Equal(t, "\x00\x01\x02key\xff", got.Content)
			},
		},
		{
			name:   "error result with a NUL after binary-looking bytes: text kept, only the NUL replaced",
			result: tool.Result{Content: "\x01stderr: boom\n\x00", IsError: true},
			check: func(t *testing.T, got tool.Result) {
				assert.True(t, got.IsError)
				assert.Contains(t, got.Content, "stderr: boom")
				assert.NotContains(t, got.Content, "\x00")
				assert.NotContains(t, got.Content, "[binary output omitted:")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Meta + Trusted keeps the content guard out of the way, so what
			// comes back is what executeToolRaw produced.
			tc.result.Trusted = true
			results := dispatchOnce(t, tool.KindMeta, authz.Permission{StateImpact: authz.Stateless}, tc.result)

			require.Len(t, results, 1)
			tc.check(t, results[0])
		})
	}
}

// dispatchOnce runs one call of a fake tool returning result through
// dispatchToolUses and returns what the loop would go on to record.
func dispatchOnce(t *testing.T, kind tool.Kind, perm authz.Permission, result tool.Result) []tool.Result {
	t.Helper()
	ft := &fakeDispatchTool{name: "git", kind: kind, perm: perm, result: result}
	l := &Loop{
		Tools:      []tool.Tool{ft},
		SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"},
	}
	loopWithInjectedExecutor(t, l, pipeline.NewRegistry())

	uses := []llm.ToolUseBlock{{ID: "tu-1", Name: "git", Input: json.RawMessage(`{"args":{}}`)}}
	results, _ := l.dispatchToolUses(memory.WithSystemApproval(context.Background(), "test"), uses, dispatchTestSession(), 0, 0, nil, nil)
	require.Equal(t, 1, ft.executed())
	return results
}
