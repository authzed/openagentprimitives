package setupmcpcmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// longBearer is deliberately long and distinctive so a test can catch any
// truncation/elision (e.g. a "..." middle-out summary) that a naive display
// helper might apply.
const longBearer = "oap_mcp_access_token_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ.signature-part-that-is-also-quite-long-1234567890"

func TestEmitClaudeCode(t *testing.T) {
	cases := []struct {
		name   string
		bearer string
	}{
		{name: "with bearer", bearer: longBearer},
		{name: "without bearer", bearer: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := emitClaudeCode("https://cluster.example.com", tc.bearer)

			assert.Contains(t, out, "https://cluster.example.com/mcp", "emitted URL must end in /mcp")
			assert.Contains(t, out, "claude mcp add --transport http oap https://cluster.example.com/mcp")

			if tc.bearer != "" {
				assert.Contains(t, out, tc.bearer, "bearer must appear verbatim, never truncated")
				assert.Contains(t, out, `--header "Authorization: Bearer `+tc.bearer+`"`)
			} else {
				assert.NotContains(t, out, "--header")
				assert.NotContains(t, out, "Authorization")
			}

			// The command line is followed by a blank line and then a JSON
			// snippet; extract and validate that part unmarshals cleanly.
			idx := strings.Index(out, "{")
			require.GreaterOrEqual(t, idx, 0, "expected a JSON snippet in the output")
			jsonPart := out[idx:]

			var decoded struct {
				MCPServers map[string]struct {
					Type    string            `json:"type"`
					URL     string            `json:"url"`
					Headers map[string]string `json:"headers"`
				} `json:"mcpServers"`
			}
			require.NoError(t, json.Unmarshal([]byte(jsonPart), &decoded), "JSON snippet must unmarshal cleanly")

			entry, ok := decoded.MCPServers["oap"]
			require.True(t, ok, "expected an \"oap\" entry in mcpServers")
			assert.Equal(t, "https://cluster.example.com/mcp", entry.URL)
			assert.Equal(t, "http", entry.Type)

			if tc.bearer != "" {
				require.NotNil(t, entry.Headers)
				assert.Equal(t, "Bearer "+tc.bearer, entry.Headers["Authorization"])
			} else {
				assert.Empty(t, entry.Headers)
			}
		})
	}
}

func TestEmitCursor(t *testing.T) {
	cases := []struct {
		name   string
		bearer string
	}{
		{name: "with bearer", bearer: longBearer},
		{name: "without bearer", bearer: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := emitCursor("https://cluster.example.com/", tc.bearer)

			assert.Contains(t, out, "https://cluster.example.com/mcp", "emitted URL must end in /mcp, even when serverURL had a trailing slash")
			assert.NotContains(t, out, "//mcp", "trailing slash on serverURL must not produce a doubled slash")

			var decoded struct {
				MCPServers map[string]struct {
					URL     string            `json:"url"`
					Headers map[string]string `json:"headers"`
				} `json:"mcpServers"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &decoded), "entire output must be valid JSON")

			entry, ok := decoded.MCPServers["oap"]
			require.True(t, ok, "expected an \"oap\" entry in mcpServers")
			assert.Equal(t, "https://cluster.example.com/mcp", entry.URL)

			if tc.bearer != "" {
				assert.Contains(t, out, tc.bearer, "bearer must appear verbatim, never truncated")
				require.NotNil(t, entry.Headers)
				assert.Equal(t, "Bearer "+tc.bearer, entry.Headers["Authorization"])
			} else {
				assert.NotContains(t, out, "Authorization")
				assert.Empty(t, entry.Headers)
			}
		})
	}
}

func TestEmitGeneric(t *testing.T) {
	cases := []struct {
		name   string
		bearer string
	}{
		{name: "with bearer", bearer: longBearer},
		{name: "without bearer", bearer: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := emitGeneric("https://cluster.example.com", tc.bearer)

			assert.Contains(t, out, "https://cluster.example.com/mcp")

			var decoded struct {
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &decoded), "entire output must be valid JSON")
			assert.Equal(t, "https://cluster.example.com/mcp", decoded.URL)

			if tc.bearer != "" {
				assert.Contains(t, out, tc.bearer, "bearer must appear verbatim, never truncated")
				require.NotNil(t, decoded.Headers)
				assert.Equal(t, "Bearer "+tc.bearer, decoded.Headers["Authorization"])
			} else {
				assert.NotContains(t, out, "Authorization")
				assert.Empty(t, decoded.Headers)
			}
		})
	}
}

// TestEmit_AllToolsAppendMCPSuffix is a compact cross-tool sanity pass: every
// emitter, regardless of whether serverURL already ends in a slash, must
// route the URL through the same "/mcp" suffix with no doubled slash.
func TestEmit_AllToolsAppendMCPSuffix(t *testing.T) {
	emitters := map[string]func(serverURL, bearer string) string{
		"claude-code": emitClaudeCode,
		"cursor":      emitCursor,
		"generic":     emitGeneric,
	}

	for name, emit := range emitters {
		t.Run(name, func(t *testing.T) {
			out := emit("https://cluster.example.com", "")
			assert.True(t, strings.Contains(out, "https://cluster.example.com/mcp"), "expected /mcp suffix in output")
			assert.False(t, strings.Contains(out, "mcp/mcp"), "must not double-append /mcp")
		})
	}
}
