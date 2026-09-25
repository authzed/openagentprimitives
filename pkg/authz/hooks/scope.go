package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// ToolReadsDecl is the slice of the MCPServer ToolResourceMapping.Reads the
// Scope/InfoLeak hooks consume. The runner adapts its
// *spiceboxv1alpha1.ToolResourceMapping into this (avoids the hooks importing
// the v1alpha1 types and a potential cycle).
type ToolReadsDecl struct {
	ResourceType         string
	Permission           string
	IDArg                string
	ResultIDField        string
	BypassRequesterCheck bool // InfoLeakRead uses this; Scope ignores it

	// ResultResources reports the SEVERAL resources one call's result came
	// from, each with its own slice of that result — the shape a memory search
	// across resource pools needs, and the one a ResourceType + field path
	// cannot express (see resultreads.go). Nil on every single-resource decl,
	// which is every declaration the API server can produce.
	//
	// Function-valued and supplied by the runner's built-in wiring, never by a
	// CRD. Reading a result into resources needs that result's format, which
	// the tool's own package owns; and a CRD-settable field naming plural
	// arbitrary-typed resources would let an MCPServer author decide the
	// audience of its own results — the authoring problem the
	// component-written mint route exists to avoid. Single-resource decls are
	// CRD-settable today; the rule is that we do not WIDEN that surface.
	//
	// The Scope hook ignores it: narrowing and the Disallow set are
	// single-resource questions, and a decl naming no ResourceType has nothing
	// for ResourceDisallowed to match.
	ResultResources func(result string) ([]ToolReadResource, error)
}

// ScopeDeps is the dependency struct for the Scope hook.
type ScopeDeps struct {
	Enabled     bool
	LookupReads func(toolName string) *ToolReadsDecl // nil decl ⇒ no read resource
	GetScope    func(ctx context.Context) (scope.Scope, bool, error)
	Logger      *slog.Logger
}

// Scope enforces dynamic session scope at dispatch time: narrowing / tool-deny /
// arg-constraints (via scope.CheckScope) and the hard-deny Disallow set (via
// scope.ResourceDisallowed). Always active when Enabled, regardless of
// toolCalls.mode — closing the disabled-mode gap (spec §8). Pre-fetch when the
// resource id is in args (idArg); post-fetch when it only appears in the result
// (resultIDField).
type Scope struct{ d ScopeDeps }

// NewScope creates a Scope hook.
func NewScope(d ScopeDeps) *Scope {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Scope{d: d}
}

func (h *Scope) Name() string { return "scope" }
func (h *Scope) Points() []pipeline.Point {
	return []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall}
}

func (h *Scope) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if !h.d.Enabled || in.Tool == nil || h.d.LookupReads == nil || h.d.GetScope == nil {
		return pipeline.Decision{}
	}

	// Errored results carry no resource to scope-check / taint / gate — and
	// (post toolguard) the post pipeline now runs for failures too. Skip.
	if in.Point == pipeline.PostToolCall && in.Tool != nil && in.Tool.IsError {
		return pipeline.Decision{}
	}

	// At PreToolCall, also run the narrowing/tool-deny/arg-constraint pass
	// (scope.CheckScope) — these are independent of a resolved resource id.
	if in.Point == pipeline.PreToolCall {
		sc, ok, err := h.d.GetScope(ctx)
		if err != nil {
			return pipeline.Decision{Verdict: pipeline.Deny, Reason: "session scope unavailable; refusing to dispatch"}
		}
		if ok {
			// Malformed (LLM-supplied, prompt-injectable) args MUST NOT be
			// silently evaluated against an empty map — a scope Forbid
			// constraint would then pass on garbage JSON. Treat unparseable
			// args as the fail-closed signal (consistent with deps.go's
			// Extract* helpers, which return their unmarshal error). Empty
			// args (len 0) are a legitimate no-arg call; only a non-empty,
			// non-parseable body denies.
			var argsMap map[string]any
			if len(in.Tool.Args) > 0 {
				if err := json.Unmarshal(in.Tool.Args, &argsMap); err != nil {
					h.d.Logger.Info("scope: tool args unparseable; refusing to dispatch",
						"tool", in.Tool.Name, "err", err.Error())
					return pipeline.Decision{Verdict: pipeline.Deny, Reason: "scope: tool args unparseable; refusing to dispatch"}
				}
			}
			argsMap = unwrapEnvelope(argsMap)
			if r := scope.CheckScope(sc, in.Tool.Name, jsonArgs(argsMap)); !r.OK {
				return pipeline.Decision{Verdict: pipeline.Deny, Reason: r.Message}
			}
		}
	}

	decl := h.d.LookupReads(in.Tool.Name)
	if decl == nil || decl.ResourceType == "" {
		return pipeline.Decision{}
	}

	// Resolve resource id by point: idArg at Pre, resultIDField at Post.
	var resID string
	switch in.Point {
	case pipeline.PreToolCall:
		if decl.IDArg == "" {
			return pipeline.Decision{} // post-fetch tool; nothing to check pre-execute
		}
		v, err := ExtractToolIDArg(in.Tool.Args, decl.IDArg)
		if err != nil || v == "" {
			return pipeline.Decision{}
		}
		resID = v
	case pipeline.PostToolCall:
		if decl.ResultIDField == "" {
			return pipeline.Decision{} // pre-fetch tool already checked at PreToolCall
		}
		v, err := ExtractToolResultID(in.Tool.Result, decl.ResultIDField)
		if err != nil || v == "" {
			return pipeline.Decision{}
		}
		resID = v
	default:
		return pipeline.Decision{}
	}

	sc, ok, err := h.d.GetScope(ctx)
	if err != nil {
		h.d.Logger.Info("scope-disallow: read session scope failed; failing closed",
			"tool", in.Tool.Name, "err", err.Error())
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("session scope unavailable; refusing to serve %s:%s", decl.ResourceType, resID),
		}
	}
	if !ok {
		return pipeline.Decision{}
	}
	if sc.ResourceDisallowed(decl.ResourceType, resID) {
		return pipeline.Decision{
			Verdict: pipeline.Deny,
			Reason:  fmt.Sprintf("session scope: access to %s:%s is disallowed for this session", decl.ResourceType, resID),
			Audit: []pipeline.AuditRecord{{
				Kind:   "scope_disallow_blocked",
				Fields: map[string]any{"tool": in.Tool.Name, "resource": decl.ResourceType + ":" + resID},
			}},
		}
	}
	return pipeline.Decision{}
}

var _ pipeline.Hook = (*Scope)(nil)
