package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// IntrospectConfig wires introspect_tool to the runner's tool set.
type IntrospectConfig struct {
	// Resolve maps an LLM-facing tool name to the Tool, if present.
	Resolve func(name string) (tool.Tool, bool)
	// Names returns every valid tool name, for the unknown-name error.
	Names func() []string
}

// NewIntrospect constructs the introspect_tool meta-tool.
func NewIntrospect(cfg IntrospectConfig) tool.Tool {
	return &introspectTool{cfg: cfg}
}

type introspectTool struct{ cfg IntrospectConfig }

func (*introspectTool) Name() string    { return "introspect_tool" }
func (*introspectTool) Kind() tool.Kind { return tool.KindMeta }

// Permission: read-only, no side effects, no resource touched.
func (*introspectTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*introspectTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*introspectTool) Description() string {
	return "Return the full, exact contract of one of your tools: every allowed " +
		"subcommand, flag, and positional argument (for CLI/sandbox tools) or " +
		"field and constraint (for MCP tools), including the justification for " +
		"each constraint. Call this BEFORE the first time you use a CLI/sandbox " +
		"tool — its argument list is strict and unknown flags or wrong positional " +
		"counts are rejected before execution. Pass the exact tool name as shown " +
		"in your tool list, e.g. {\"tool\": \"gitlike_gh\"}."
}

func (*introspectTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"tool": {
				"type": "string",
				"description": "Exact name of the tool to describe, as it appears in your tool list (e.g. \"gitlike_gh\")."
			}
		},
		"required": ["tool"]
	}`)
}

func (t *introspectTool) Execute(ctx context.Context, args json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var in struct {
		Tool string `json:"tool"`
	}
	if res, ok := tool.ParseArgs(args, &in, t.Name(), `{"tool": "gitlike_gh"}`); !ok {
		return res, nil
	}
	name := strings.TrimSpace(in.Tool)
	if name == "" {
		return tool.Result{Content: "introspect_tool: `tool` is required — pass the exact tool name from your tool list.", IsError: true, Trusted: true}, nil
	}

	target, ok := t.cfg.Resolve(name)
	if !ok {
		return tool.Result{Content: t.unknown(name), IsError: true, Trusted: true}, nil
	}
	intro, ok := target.(tool.Introspectable)
	if !ok {
		return tool.Result{Content: fmt.Sprintf(
			"introspect_tool: %q has no additional contract — its Description and input schema already describe it fully.", name),
			Trusted: true}, nil
	}
	body, err := intro.Introspect()
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("introspect_tool: could not describe %q: %v", name, err), IsError: true, Trusted: true}, nil
	}
	return tool.Result{Content: body, Trusted: true}, nil
}

// unknown builds the error shown when the requested tool name is not
// in the agent's tool set — it lists the valid names so the model can
// correct itself in one turn.
func (t *introspectTool) unknown(name string) string {
	var valid []string
	if t.cfg.Names != nil {
		valid = append(valid, t.cfg.Names()...)
		sort.Strings(valid)
	}
	msg := fmt.Sprintf("introspect_tool: no tool named %q.", name)
	if len(valid) > 0 {
		msg += " Valid tool names: " + strings.Join(valid, ", ") + "."
	}
	return msg
}
