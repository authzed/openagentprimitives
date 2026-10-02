package setupmcpcmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

// mcpURL appends "/mcp" to serverURL, trimming any trailing slash first so
// the result never contains a doubled slash. Every emitter routes the URL it
// prints through this function, so "the emitted URL always ends in /mcp" is
// enforced in one place rather than re-derived per tool.
func mcpURL(serverURL string) string {
	return strings.TrimRight(serverURL, "/") + "/mcp"
}

// authHeaders returns the single Authorization header map for bearer, or nil
// when bearer is empty. nil (not an empty map) so callers can tag the field
// `omitempty` and have it vanish entirely from unauthenticated output, rather
// than rendering as `"headers": {}`.
func authHeaders(bearer string) map[string]string {
	if bearer == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + bearer}
}

// mcpServerEntry is the common shape both Claude Code's and Cursor's
// mcpServers map use.
type mcpServerEntry struct {
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// emitClaudeCode renders the `claude mcp add` command for this server plus
// the equivalent mcpServers JSON snippet, for a user who'd rather hand-edit
// their config. bearer, when non-empty, is embedded verbatim (never
// truncated) as an Authorization header in both the --header flag and the
// JSON snippet.
func emitClaudeCode(serverURL, bearer string) string {
	url := mcpURL(serverURL)

	cmd := fmt.Sprintf("claude mcp add --transport http oap %s", url)
	if bearer != "" {
		cmd += fmt.Sprintf(" --header %s", quoteShellArg(fmt.Sprintf("Authorization: Bearer %s", bearer)))
	}

	snippet := struct {
		MCPServers map[string]mcpServerEntry `json:"mcpServers"`
	}{
		MCPServers: map[string]mcpServerEntry{
			"oap": {
				Type:    "http",
				URL:     url,
				Headers: authHeaders(bearer),
			},
		},
	}
	b, err := json.MarshalIndent(snippet, "", "  ")
	if err != nil {
		// snippet is a fixed shape of strings and maps of strings; it cannot fail
		// to marshal. Panic rather than silently drop the JSON half of the output.
		panic(fmt.Sprintf("setupmcpcmd: marshal claude-code snippet: %v", err))
	}

	return cmd + "\n\n" + string(b) + "\n"
}

// emitCursor renders the .cursor/mcp.json snippet for this server. The whole
// output is valid JSON.
func emitCursor(serverURL, bearer string) string {
	url := mcpURL(serverURL)

	snippet := struct {
		MCPServers map[string]mcpServerEntry `json:"mcpServers"`
	}{
		MCPServers: map[string]mcpServerEntry{
			"oap": {
				URL:     url,
				Headers: authHeaders(bearer),
			},
		},
	}
	b, err := json.MarshalIndent(snippet, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("setupmcpcmd: marshal cursor snippet: %v", err))
	}
	return string(b) + "\n"
}

// emitGeneric renders a minimal {"url": ..., "headers": {...}} snippet for any
// MCP client that doesn't have a dedicated emitter. The whole output is valid
// JSON.
func emitGeneric(serverURL, bearer string) string {
	snippet := struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
	}{
		URL:     mcpURL(serverURL),
		Headers: authHeaders(bearer),
	}
	b, err := json.MarshalIndent(snippet, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("setupmcpcmd: marshal generic snippet: %v", err))
	}
	return string(b) + "\n"
}

// quoteShellArg wraps s in double quotes for display in a copy-pasteable
// shell command line. s here is always "Authorization: Bearer <token>",
// which contains no double quotes or backslashes, so a full shell-quoting
// library is not needed — this only ever has to survive display, not
// execution.
func quoteShellArg(s string) string {
	return `"` + s + `"`
}
