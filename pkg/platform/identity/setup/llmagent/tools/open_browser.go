// Package tools holds the LLM-tool implementations for the setup agent. Each
// tool is a trio of package-level identifiers — <Name>Name, <Name>Schema, and a
// <Name>Run function — that agent.go adapts to tool.Tool. Package-level seams
// (HTTP clients, prompters, confirmers) allow test injection. Browser opening
// is not among them: pkg/x/browser owns that seam and suppresses itself inside
// a test binary, so there is nothing here for a test to have to remember.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/authzed/openagentprimitives/pkg/x/browser"
)

// OpenBrowserName is the LLM-facing tool name.
const OpenBrowserName = "open_browser"

// OpenBrowserSchema is the JSON Schema the LLM sees.
const OpenBrowserSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "url": { "type": "string", "description": "http or https URL to open in the user's default browser" }
  },
  "required": ["url"]
}`

// OpenBrowserRun is the runtime handler: it validates the URL scheme, opens the
// URL, and returns a short success message.
//
// It calls browser.Open directly rather than through a package-level opener
// var. The var existed so a test could avoid launching a window; browser.Open
// declines to launch one inside a test binary on its own, which covers the test
// that forgets as well as the one that remembers.
func OpenBrowserRun(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("open_browser: parse args: %w", err)
	}
	u, err := url.Parse(args.URL)
	if err != nil {
		return "", fmt.Errorf("open_browser: invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("open_browser: only http/https schemes allowed (got %q)", u.Scheme)
	}
	if err := browser.Open(args.URL); err != nil {
		return "", fmt.Errorf("open_browser: %w", err)
	}
	return fmt.Sprintf("opened %s", args.URL), nil
}
