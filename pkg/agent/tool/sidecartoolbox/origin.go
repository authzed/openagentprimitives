// origin.go: tags sidecar-synthesized tools with their sidecar origin so
// toolguard's origin-level circuit breaker treats all tools of one sidecar
// as sharing health. Replaces the former degrade.go tracker (a
// non-recovering 3-strike short-circuit): the toolguard origin breaker
// covers the same wedged-sidecar mode with backoff recovery and per-class
// configurability.
package sidecartoolbox

import (
	"context"
	"encoding/json"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// originTool wraps a synthesized MCP tool, overriding its origin: these
// tools belong to the sidecar, not a real MCPServer CR.
type originTool struct {
	inner      agenttool.Tool
	sidecarRef string
}

func (o *originTool) Name() string                 { return o.inner.Name() }
func (o *originTool) Kind() agenttool.Kind         { return o.inner.Kind() }
func (o *originTool) Description() string          { return o.inner.Description() }
func (o *originTool) InputSchema() json.RawMessage { return o.inner.InputSchema() }
func (o *originTool) Permission() authz.Permission { return o.inner.Permission() }
func (o *originTool) PermissionVariants() []authz.PermissionVariant {
	return o.inner.PermissionVariants()
}
func (o *originTool) Execute(ctx context.Context, raw json.RawMessage, sess *agenttool.SessionContext) (agenttool.Result, error) {
	return o.inner.Execute(ctx, raw, sess)
}

// Origin implements agenttool.OriginTool.
func (o *originTool) Origin() string { return "sidecartoolbox/" + o.sidecarRef }

// Cancel forwards to inner's Cancel when inner implements tool.Cancellable
// (the synthesized sidecar tool always wraps an *mcp.MCPTool, which does),
// so sidecartoolbox tools are as interruptible as plain MCP tools. inner is
// a field, not an embedded type, so Cancel is NOT promoted automatically —
// this forwarder is required. Degrades to a no-op (never an error) if inner
// doesn't implement it, matching Cancellable's optional-capability contract.
func (o *originTool) Cancel(ctx context.Context) error {
	if c, ok := o.inner.(agenttool.Cancellable); ok {
		return c.Cancel(ctx)
	}
	return nil
}
