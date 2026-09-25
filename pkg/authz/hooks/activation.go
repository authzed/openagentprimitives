package hooks

import "github.com/authzed/openagentprimitives/pkg/platform/pipeline"

// activation.go is the SHARED activation+order table consulted by both the
// runner's (*Loop).buildPipelineRegistry and the `oap agent authz` rendered
// view, so the registry and the view cannot drift. ActivationConfig is a small
// import-clean struct (mode strings / bools / counts) the CLI and Loop both
// populate from the AgentClass — the table never imports pkg/apis/v1alpha1, so
// the activation logic stays decoupled from the CRD shape.

// Activation is the tri-state activation of a hook for a class-level view.
type Activation int

const (
	// ActiveNo means the hook is definitely not registered for this config.
	ActiveNo Activation = iota
	// ActiveYes means the hook is definitely registered for this config.
	ActiveYes
	// ActiveConditional means activation depends on per-session runtime state
	// the class-level config cannot determine (today: McpTrust, which is active
	// iff the live session has MCP tools).
	ActiveConditional
)

func (a Activation) String() string {
	switch a {
	case ActiveYes:
		return "active"
	case ActiveConditional:
		return "conditional"
	default:
		return "inactive"
	}
}

// HookDescriptor is one row of the rendered authz pipeline view.
type HookDescriptor struct {
	Name   string
	Points []pipeline.Point
	Active Activation
	// Note annotates a conditional/active row with the reason (e.g. the
	// session-tool dependency for McpTrust). Empty for plain rows.
	Note string
}

// ActivationConfig is the neutral projection of an AgentClass's authz config the
// activation table consults. The CLI and the runner both populate it.
type ActivationConfig struct {
	// ToolCallMode is authz.toolCalls.mode ("enforcing" | "permissive" |
	// "disabled" | ""). Anything other than "disabled"/"" activates ToolCallAuthz.
	ToolCallMode string
	// ScopeEnabled is authz.scope.enabled.
	ScopeEnabled bool
	// ColdStart is authz.scope.coldStart ("extractAndApprove" |
	// "extractAndAutoApply" | "off" | ""). Anything other than "off"/"" with
	// ScopeEnabled activates ColdStartScope.
	ColdStart string
	// LeakageMode is authz.informationLeakage.mode ("enforcing" | "logging" |
	// "disabled" | ""). Anything other than "disabled"/"" activates the two
	// information-leakage hooks.
	LeakageMode string
	// InteractPermission is authz.session.interactPermission. Non-empty
	// activates the Interact gate.
	InteractPermission string
	// BoundEntityCount is len(spec.boundEntities). >0 activates EntityBind.
	BoundEntityCount int
}

func boolToActivation(b bool) Activation {
	if b {
		return ActiveYes
	}
	return ActiveNo
}

func enabledMode(mode string) bool {
	return mode != "" && mode != "disabled"
}

// ActiveHooks returns the ordered hook descriptors for cfg. The list is a flat
// projection across all pipeline points, ordered by the point sequence
// (SessionStart → InboundTurn → PreToolCall → Post/PreResponse → SessionEnd) and
// then by the code-owned per-point Order* constants. Each descriptor's Active
// is the class-level activation; McpTrust is ActiveConditional because its
// activation depends on per-session MCP-tool presence.
func ActiveHooks(cfg ActivationConfig) []HookDescriptor {
	leakageOn := enabledMode(cfg.LeakageMode)
	coldStartOn := cfg.ScopeEnabled && cfg.ColdStart != "" && cfg.ColdStart != "off"

	return []HookDescriptor{
		{
			Name:   "cold_start_scope",
			Points: []pipeline.Point{pipeline.SessionStart},
			Active: boolToActivation(coldStartOn),
		},
		{
			Name:   "interact",
			Points: []pipeline.Point{pipeline.InboundTurn},
			Active: boolToActivation(cfg.InteractPermission != ""),
		},
		{
			Name:   "entity_bind",
			Points: []pipeline.Point{pipeline.InboundTurn},
			Active: boolToActivation(cfg.BoundEntityCount > 0),
		},
		{
			Name:   "mcp_trust",
			Points: []pipeline.Point{pipeline.PreToolCall},
			Active: ActiveConditional,
			Note:   "active only when the session has MCP tools",
		},
		{
			Name:   "tool_call_authz",
			Points: []pipeline.Point{pipeline.PreToolCall},
			Active: boolToActivation(enabledMode(cfg.ToolCallMode)),
		},
		{
			Name:   "scope",
			Points: []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall},
			Active: boolToActivation(cfg.ScopeEnabled),
		},
		{
			Name:   "info_leak_read",
			Points: []pipeline.Point{pipeline.PostToolCall},
			Active: boolToActivation(leakageOn),
		},
		{
			Name:   "info_leak_audience",
			Points: []pipeline.Point{pipeline.PostToolCall, pipeline.PreResponse},
			Active: boolToActivation(leakageOn),
		},
		{
			Name:   "session_cleanup",
			Points: []pipeline.Point{pipeline.SessionEnd},
			Active: ActiveYes,
		},
	}
}
