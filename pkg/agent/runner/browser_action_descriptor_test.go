package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// TestBuildToolCallApprovalAsk_BrowserDescriptionWinsOverSummarizer pins the
// fix for the wire-level "Click the element with ref e13 on httpbin.org" card
// text: when the browser acting tool's NamedArgs has stamped
// browserschema.WebActionDescriptionArg (built from ActionFacts — see
// pkg/agent/tool/browser/action_descriptor.go), buildToolCallApprovalAsk uses
// it VERBATIM as the card's Summary and never calls the summarizer LLM at
// all — the descriptor is cheaper and more accurate (it has the real DOM
// facts; the summarizer only ever saw "ref: e13").
func TestBuildToolCallApprovalAsk_BrowserDescriptionWinsOverSummarizer(t *testing.T) {
	l := loopForApprovalCard(t)
	cap := &captureSummarizer{}
	l.ApprovalSummarizer = cap

	args := map[string]any{
		"ref":                                 "e13",
		"host":                                "httpbin.org",
		browserschema.WebActionDescriptionArg: `Select "medium"`,
	}
	ask, err := l.buildToolCallApprovalAsk(
		memory.WithSystemApproval(context.Background(), "test"),
		"browser_click", args, authz.Permission{StateImpact: authz.External},
		"tu-1", "", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.NoError(t, err)
	require.NotNil(t, ask)

	assert.Equal(t, `Select "medium"`, ask.Summary,
		"the browser descriptor must be used verbatim as the card's What line")
	assert.Zero(t, cap.last, "the summarizer LLM must never be invoked when a browser descriptor is stamped")

	// The descriptor is an internal gate-derived presentation value, exactly
	// like web_action_key/fingerprint — it must not leak into the args the
	// card displays, the durable record, or the caller's own map.
	argsJSON, _ := ask.Payload["args_json"].(string)
	assert.NotContains(t, argsJSON, "Select",
		"the descriptor must be stripped before the displayed args are rendered")
	argsMap, _ := ask.Payload["args_map"].(map[string]any)
	assert.NotContains(t, argsMap, browserschema.WebActionDescriptionArg,
		"the descriptor must be stripped from args_map too")
	assert.NotContains(t, args, browserschema.WebActionDescriptionArg,
		"the caller's own map must end up stripped as well (same map, mutated in place)")
}

// TestBuildToolCallApprovalAsk_NoBrowserDescriptionFallsBackToSummarizer pins
// the other half: a call with no stamped descriptor (a non-browser tool, or a
// browser call where NamedArgs never ran) is unaffected and still goes
// through the ordinary summarizer LLM path.
func TestBuildToolCallApprovalAsk_NoBrowserDescriptionFallsBackToSummarizer(t *testing.T) {
	l := loopForApprovalCard(t)
	l.Tools = []tool.Tool{&secretArgTool{name: "some_mcp_tool"}}
	cap := &captureSummarizer{}
	l.ApprovalSummarizer = cap

	args := map[string]any{"repo": "demo/widgets"}
	ask, err := l.buildToolCallApprovalAsk(
		memory.WithSystemApproval(context.Background(), "test"),
		"some_mcp_tool", args, authz.Permission{StateImpact: authz.External},
		"tu-1", "", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.NoError(t, err)
	require.NotNil(t, ask)

	assert.Equal(t, "summary", ask.Summary, "absent a browser descriptor, the summarizer's output is used as before")
	assert.Equal(t, "some_mcp_tool", cap.last.Tool, "the summarizer LLM must still be invoked for a non-browser call")
}
