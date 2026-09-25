package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// perCallTool stands in for a sandbox tool: a passthrough base with a per-call
// answer that differs by argv.
type perCallTool struct {
	echoTool
	got map[string]any
}

func (p *perCallTool) Name() string { return "gitlike_gh" }
func (p *perCallTool) PermissionForCall(args map[string]any) (authz.Permission, bool) {
	p.got = args
	if a, _ := args["args"].([]any); len(a) > 1 && a[1] == "create" {
		return authz.Permission{StateImpact: authz.External,
			Check: &authz.PermissionCheck{ResourceType: "github_repo", Permission: "write"}}, true
	}
	return authz.Permission{StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{ResourceType: "github_repo", Permission: "read"}}, true
}

// The whole point of the fix: which permission applies must depend on the CALL,
// not just the tool. A tool resolving one permission for every argv is how
// `gh pr create` came to be authorized as though it were `gh pr view`.
func TestResolvePermission_prefersThePerCallAnswer(t *testing.T) {
	tl := &perCallTool{}
	l := &Loop{AgentName: "demo-agent", Tools: []tool.Tool{tl}}
	deps := l.toolCallAuthzDeps()

	write, err := deps.ResolvePermission("gitlike_gh", map[string]any{
		"args": []any{"pr", "create", "--title", "x"},
	})
	require.NoError(t, err)
	assert.Equal(t, authz.External, write.StateImpact,
		"pr create must resolve to its own severity, not the tool's base")

	read, err := deps.ResolvePermission("gitlike_gh", map[string]any{
		"args": []any{"pr", "view", "123"},
	})
	require.NoError(t, err)
	assert.Equal(t, authz.Readonly, read.StateImpact)
}

// A tool that cannot answer per-call must fall through unchanged — MCP tools
// keep resolving through their CEL variants.
func TestResolvePermission_fallsBackForToolsWithoutAPerCallAnswer(t *testing.T) {
	l := &Loop{AgentName: "demo-agent", Tools: []tool.Tool{echoTool{name: "plain"}}}
	deps := l.toolCallAuthzDeps()

	got, err := deps.ResolvePermission("plain", map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, authz.Passthrough, got.StateImpact)
}

// namedArgsTool contributes a parsed view of an argv call, as a sandbox tool
// does.
type namedArgsTool struct{ perCallTool }

func (namedArgsTool) NamedArgs(args map[string]any) (map[string]any, bool) {
	a, _ := args["args"].([]any)
	for i := 0; i+1 < len(a); i++ {
		if a[i] == "--repo" {
			return map[string]any{"repo": a[i+1]}, true
		}
	}
	return nil, false
}

// The check reads NAMED arguments; a sandbox call carries an argv array. Unless
// the tool's parsed view reaches the args, resourceIDTemplate "{repo}" resolves
// against nothing and every per-resource check on a sandbox tool fails — which
// is what stops a slot from ever binding an instance.
//
// Asserted on NormalizeArgs rather than BuildInputs, and that move is the point.
// The conversion used to live inside BuildInputs, so only the CHECK saw the
// parsed view while the approval ask was handed the raw envelope — it resolved
// `{remote}` against a map with no remote and produced an empty resource id.
// Live, that made an approved push unbindable: the card showed the URL, the
// human approved, and channelsd refused the decision. Normalizing once, before
// anything reads the args, is what makes one call have one view.
func TestNormalizeArgs_rendersTheToolsParsedViewForEveryConsumer(t *testing.T) {
	tl := &namedArgsTool{}
	l := &Loop{AgentName: "demo-agent", Tools: []tool.Tool{tl}}
	deps := l.toolCallAuthzDeps()

	in := pipeline.Input{Tool: &pipeline.ToolCallInfo{Name: "gitlike_gh"}}
	raw := map[string]any{"args": []any{"pr", "view", "--repo", "demo-org/demo-repo"}}

	named := deps.NormalizeArgs(in, raw)
	assert.Equal(t, "demo-org/demo-repo", named["repo"],
		"the parsed view must reach the args, or {repo} has nothing to resolve")

	// And the check still receives it, since the hook normalizes before
	// BuildInputs runs.
	got := deps.BuildInputs(in, named, authz.Permission{
		StateImpact: authz.Readonly,
		Check:       &authz.PermissionCheck{ResourceType: "github_repo", Permission: "read"},
	})
	assert.Equal(t, "demo-org/demo-repo", got.Args["repo"])
}

// A tool with no parsed view must pass its args through untouched — MCP tools
// already arrive named.
func TestNormalizeArgs_passesThroughForToolsWithoutAParsedView(t *testing.T) {
	l := &Loop{AgentName: "demo-agent", Tools: []tool.Tool{echoTool{name: "mcp_thing"}}}
	deps := l.toolCallAuthzDeps()

	in := pipeline.Input{Tool: &pipeline.ToolCallInfo{Name: "mcp_thing"}}
	raw := map[string]any{"companyId": "acme"}

	assert.Equal(t, raw, deps.NormalizeArgs(in, raw),
		"an MCP tool's args are already named; normalization must not disturb them")
}
