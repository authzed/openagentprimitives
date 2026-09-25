package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner/approval/summarizer"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// secretArgTool declares one of its argument paths sensitive, the way an
// MCPServer's tools[].args.sensitiveFields does.
type secretArgTool struct {
	name      string
	sensitive []string
}

func (f *secretArgTool) Name() string                 { return f.name }
func (f *secretArgTool) Kind() tool.Kind              { return tool.KindMCP }
func (f *secretArgTool) Description() string          { return "does a thing" }
func (f *secretArgTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (f *secretArgTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}
func (f *secretArgTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.External}
}
func (f *secretArgTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (f *secretArgTool) SensitiveArgFields() []string                  { return f.sensitive }

// captureSummarizer records the Request the approval flow hands the isolated
// summarizer LLM, so a test can assert what that LLM is allowed to see.
type captureSummarizer struct{ last summarizer.Request }

func (c *captureSummarizer) Name() string { return "capture" }
func (c *captureSummarizer) Summarize(_ context.Context, r summarizer.Request) (string, error) {
	c.last = r
	return "summary", nil
}
func (c *captureSummarizer) SummarizeAnnotations(context.Context, summarizer.AnnotationRequest) (string, error) {
	return "", nil
}

func loopForApprovalCard(t *testing.T) *Loop {
	t.Helper()
	key := memory.NamespacedName{Namespace: "default", Name: "card1"}
	return &Loop{
		Mem:        memory.NewLocal(inmem.NewBackend()),
		SessionKey: key,
	}
}

// TestBuildToolCallApprovalAsk_SensitiveArgsRedactedOnCard asserts the approval
// card's display surfaces (args_json, which reaches the Slack Show-Details
// modal and the append-only memapproval record, and the summarizer request)
// carry no byte of a declared-sensitive argument, while args_map — the
// in-process value the post-approval re-check and grant write consume — stays
// raw.
func TestBuildToolCallApprovalAsk_SensitiveArgsRedactedOnCard(t *testing.T) {
	const secret = "sk-live-super-secret-value"

	l := loopForApprovalCard(t)
	l.Tools = []tool.Tool{&secretArgTool{name: "do_thing", sensitive: []string{"api_key"}}}
	cap := &captureSummarizer{}
	l.ApprovalSummarizer = cap

	args := map[string]any{"api_key": secret, "repo": "demo/widgets"}
	ask, err := l.buildToolCallApprovalAsk(
		memory.WithSystemApproval(context.Background(), "test"),
		"do_thing", args, authz.Permission{StateImpact: authz.External},
		"tu-1", "because", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.NoError(t, err)
	require.NotNil(t, ask)

	argsJSON, _ := ask.Payload["args_json"].(string)
	assert.NotContains(t, argsJSON, secret,
		"args_json reaches the approver's Show Details modal and the append-only approval record")
	assert.Contains(t, argsJSON, "demo/widgets",
		"only the declared-sensitive path is redacted; the rest of the card must stay legible")
	assert.NotContains(t, cap.last.ArgsJSON, secret,
		"the summarizer LLM prompt must not carry a declared-sensitive value")

	argsMap, _ := ask.Payload["args_map"].(map[string]any)
	require.NotNil(t, argsMap)
	assert.Equal(t, secret, argsMap["api_key"],
		"args_map stays in-process and feeds the post-approval re-check; redacting it would break execution")
	assert.Equal(t, secret, args["api_key"],
		"the caller's args map must not be mutated")
}

// TestBuildToolCallApprovalAsk_UnresolvableResourceIDFailsClosed asserts that a
// resource id the permission cannot resolve refuses to build the ask, rather
// than silently routing the approval to the session approve-set — which, for
// stateImpact: external, is the only authorization the call ever gets.
func TestBuildToolCallApprovalAsk_UnresolvableResourceIDFailsClosed(t *testing.T) {
	l := loopForApprovalCard(t)
	l.Tools = []tool.Tool{&secretArgTool{name: "do_thing"}}

	perm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       "repo",
			Permission:         "admin",
			ResourceIDTemplate: "{missing_arg}",
		},
	}
	_, err := l.buildToolCallApprovalAsk(
		memory.WithSystemApproval(context.Background(), "test"),
		"do_thing", map[string]any{"repo": "demo/widgets"}, perm,
		"tu-1", "because", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.Error(t, err, "an unresolvable resource id must not fall back to the session approve-set")
	assert.True(t, strings.Contains(err.Error(), "resource"),
		"the error should name the unresolvable resource id; got %q", err.Error())
}
