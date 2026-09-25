// server.go turns one apiadapter.Config into an MCP tool surface: one tool
// per configured operation, its schema derived from the operation's own
// params, and a call routed straight through the engine (Task 1,
// pkg/tools/apiadapter) to the upstream API. This file never reads an
// environment variable — main.go owns the env contract and hands this Server
// an already-parsed Config and an already-resolved credential.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/authzed/openagentprimitives/pkg/tools/apiadapter"
)

// Server serves one adapter config as MCP tools — one tool per operation.
type Server struct {
	cfg  apiadapter.Config
	exec *apiadapter.Executor
}

// NewServer binds cfg to an Executor built from cfg and credential. credential
// is empty when cfg.Auth.Type is "none".
func NewServer(cfg apiadapter.Config, credential string) *Server {
	return &Server{cfg: cfg, exec: apiadapter.NewExecutor(cfg, credential)}
}

// Register adds one MCP tool per configured operation. The tool's input
// schema is DERIVED from the operation's params, so the surface a model sees
// is exactly what the config declares — there is no hand-written tool here.
func (s *Server) Register(mcpSrv *mcp.Server) {
	for _, op := range s.cfg.Operations {
		mcpSrv.AddTool(&mcp.Tool{
			Name:        op.Name,
			Description: op.Description,
			InputSchema: op.InputSchema(),
		}, s.handlerFor(op))
	}
}

// handlerFor builds the tools/call handler for one operation. op is closed
// over by value (the range var in Register), so each handler stays bound to
// its own operation regardless of how many operations the config declares.
func (s *Server) handlerFor(op apiadapter.Operation) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if err := decodeArgs(req, &args); err != nil {
			return toolErr("%s: decode arguments: %v", op.Name, err), nil
		}
		res, err := s.exec.Call(ctx, op.Name, args)
		if err != nil {
			// A binding / destination / transport failure. Returned as a tool
			// error (never a bare Go error to the transport) so the model is
			// told plainly and the turn continues — CLAUDE.md's
			// no-silent-errors rule.
			return toolErr("%s: %v", op.Name, err), nil
		}
		if res.Status < 200 || res.Status >= 300 {
			// The upstream answered and said no. Surface ITS words verbatim
			// rather than a generic failure, so the model can act on them.
			return toolErr("%s: upstream returned %d: %s", op.Name, res.Status, string(res.Body)), nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(res.Body)}}}, nil
	}
}

// decodeArgs decodes req's raw MCP tool-call arguments into out. This
// adapter is a LEAF MCP server: the operation_id/_reason/args envelope a
// SidecarToolbox-sourced tool sees on the LLM-facing schema is unwrapped by
// the runner's MCP dispatcher before the call ever reaches here, so
// req.Params.Arguments is this tool's own flat argument shape, never the
// envelope. A nil Params or empty Arguments (a tool called with no args at
// all) leaves out at its zero value rather than erroring, mirroring
// encoding/json's own treatment of a missing field.
func decodeArgs(req *mcp.CallToolRequest, out any) error {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return nil
	}
	return json.Unmarshal(req.Params.Arguments, out)
}

// toolErr builds a plain-text IsError CallToolResult. Every tool-call
// failure in this binary returns one of these, never a bare Go error to the
// transport — per CLAUDE.md's no-silent-errors rule.
func toolErr(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}
