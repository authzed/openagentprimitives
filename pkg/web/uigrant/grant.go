// Package uigrant computes which tools an agent-defined UI may invoke directly
// from a browser.
//
// Materialize is the REAL answer: the fail-closed intersection of three
// independently-authored conditions, resolved by the runner against live
// per-session data at session start:
//
//  1. the UI REQUESTED it            (AgentUI.spec.tools — a bundle author's ask)
//  2. its ORIGIN PERMITS app calls   (mcpUiAppTools.enabled on the MCPServer or
//     SidecarToolbox, plus app visibility on the
//     tool itself — the origin owner's opt-in)
//  3. the DEPLOYMENT GRANTED it      (AgentClass.spec.agentUI.grantedTools)
//
// A bundle asking for a tool is not the same as being allowed to call it, which
// is why (1) and (3) are separate objects written by separate people.
//
// Ceiling computes only (1) and (3) — the static, object-level facts a
// namespace-scoped controller or an offline installer can see without a live
// session. It is deliberately NOT an authorization answer: a tool in its result
// can still be denied by (2), which only the runner can evaluate, since which
// origins exist for an AgentUI depends on the bundle a session actually runs.
// It exists so the AgentUI controller (status.eligibleTools) and the installer
// display share one two-party computation rather than each faking it — e.g. by
// handing Materialize a synthetic always-permits Origin — and drifting apart.
//
// This package takes plain slices rather than CRD types so it is testable
// without a scheme and so pkg/apis/v1alpha1's consumers never inherit grant
// logic. Three callers share it — the controller observes (Ceiling), the runner
// enforces (Materialize), the installer displays (Ceiling) — and any two would
// drift if it lived inside the third.
package uigrant

import (
	"fmt"
	"slices"
)

// Origin is one tool source and its app-visibility posture.
type Origin struct {
	// Name is the origin's bare CR name ("github"), NOT the "<kind>/<name>"
	// form tool.OriginTool.Origin() returns ("mcpserver/github"). Used to
	// name an origin in Explain's messages and to resolve an appVisible
	// collision; it never appears inside a tool name (see AppVisibleTools).
	Name string

	// AppToolsEnabled mirrors mcpUiAppTools.enabled. False ⇒ none of this
	// origin's tools are browser-callable, whatever else says otherwise —
	// and, per appVisible, it VETOES the same tool name if another origin
	// also claims it and has opted in.
	AppToolsEnabled bool

	// AppVisibleTools are the tools this origin marks app-visible, in the
	// LLM-visible origin-prefixed form ("<ref>_<tool>", e.g. "crm_list_leads")
	// — the same vocabulary requested and granted use. The bare upstream name
	// (MCPServerTool.Name) instead would make every result of this package
	// match zero keys in the runner's Loop.AppTools registry.
	AppVisibleTools []string
}

// Materialize returns the sorted, deduplicated set of tool names callable from
// the browser. Absence from ANY condition removes a tool; an empty or nil
// requested, origins, or granted input yields nil, since each condition's own
// check independently fails closed against an empty set.
//
// requested, AppVisibleTools, and granted all share one vocabulary: the
// LLM-visible, origin-prefixed tool name ("<ref>_<tool>", e.g.
// "crm_list_leads") — the exact key the runner's Loop.AppTools registry uses
// and that AppToolCallRequest.ToolName carries (pkg/agent/runner/loop.go,
// pkg/channels/channelevents/app_tool_call.go). With no translation layer
// between this result and that registry, the two cannot drift. It is also
// unambiguous across origins (two origins can share an upstream tool name but
// never a prefixed one) and is what an operator sees in approval cards and
// audit entries. The bare upstream name would match zero keys there —
// fail-closed, but silently, which is worse than a loud rejection.
//
// A tool missing from the result is simply not registered, and
// HandleAppToolCall's fail-closed lookup rejects it — no new gate.
//
// Sorted output keeps registry construction deterministic run to run.
func Materialize(requested []string, origins []Origin, granted []string) []string {
	permitted := appVisible(origins)
	grantedSet := make(map[string]struct{}, len(granted))
	for _, g := range granted {
		grantedSet[g] = struct{}{}
	}

	var out []string
	seen := map[string]struct{}{}
	for _, r := range requested {
		if _, dup := seen[r]; dup {
			continue
		}
		if _, ok := permitted[r]; !ok {
			continue
		}
		if _, ok := grantedSet[r]; !ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

// Ceiling returns the sorted, deduplicated set of tool names a UI requested AND
// some deployment granted — Materialize's conditions (1) and (3), the static
// facts computable without a live session.
//
// Ceiling is NOT an authorization answer and must never be read as one. A tool
// in its result is not thereby callable: a real call still needs condition (2),
// which only the runner can evaluate against live-resolved origins. Its callers
// are the ones needing the honestly-scoped two-party value — the AgentUI
// controller's status.eligibleTools observation, and the installer's display.
//
// An empty or nil requested or granted input yields nil. "Closed" here bounds
// the ACCEPT direction only: nothing is ever wrongly included, but denial is
// not complete, since condition (2) is never evaluated.
//
// Sorted output is load-bearing: this lands in AgentUI's status.eligibleTools,
// and unstable ordering would rewrite the object on every reconcile.
func Ceiling(requested, granted []string) []string {
	grantedSet := make(map[string]struct{}, len(granted))
	for _, g := range granted {
		grantedSet[g] = struct{}{}
	}

	var out []string
	seen := map[string]struct{}{}
	for _, r := range requested {
		if _, dup := seen[r]; dup {
			continue
		}
		if _, ok := grantedSet[r]; !ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

// ExplainCeiling returns, for each requested tool absent from Ceiling's result,
// why — the two-party analog of Explain, restricted to the one condition
// Ceiling can evaluate (the deployment grant). A tool that made the cut is
// absent from the returned map.
//
// It never attributes a miss to origin app-visibility. Explain needs a real
// Origins slice to speak to that condition, and the tempting shortcut — calling
// it with a synthetic always-permits Origin — would launder an unevaluated
// condition into a message that reads as evaluated. An operator reading "origin
// has not enabled mcpUiAppTools" when the truth is "this layer never checked"
// debugs the wrong thing. So this says one of two things: nothing, or "not in
// the deployment grant".
func ExplainCeiling(requested, granted []string) map[string]string {
	grantedSet := make(map[string]struct{}, len(granted))
	for _, g := range granted {
		grantedSet[g] = struct{}{}
	}
	out := map[string]string{}
	seen := map[string]struct{}{}
	for _, r := range requested {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		if _, ok := grantedSet[r]; !ok {
			out[r] = fmt.Sprintf("%q is not in the deployment grant on any AgentClass referencing this AgentUI", r)
		}
	}
	return out
}

// Explain returns, for each requested tool that did NOT make the three-way
// intersection, the first condition it failed, so a denial is diagnosable
// against the REAL grant. The AgentUI controller does NOT call this — it has no
// Origins slice to evaluate condition (2) against and uses ExplainCeiling
// instead. A tool that passed is absent from the map.
func Explain(requested []string, origins []Origin, granted []string) map[string]string {
	permitted := appVisible(origins)
	known := map[string]struct{}{}
	// originOf names, for a tool's "has not enabled mcpUiAppTools" message,
	// the first origin (in caller order) listing it as app-visible. Under an
	// appVisible collision that is one of the claimants, not necessarily the
	// one that vetoed — still an actionable starting point.
	originOf := map[string]string{}
	for _, o := range origins {
		for _, t := range o.AppVisibleTools {
			known[t] = struct{}{}
			if _, already := originOf[t]; !already {
				originOf[t] = o.Name
			}
		}
	}
	grantedSet := make(map[string]struct{}, len(granted))
	for _, g := range granted {
		grantedSet[g] = struct{}{}
	}

	out := map[string]string{}
	for _, r := range requested {
		if _, isKnown := known[r]; !isKnown {
			out[r] = fmt.Sprintf("%q is on no origin known to this session", r)
			continue
		}
		if _, ok := permitted[r]; !ok {
			out[r] = fmt.Sprintf("%q's origin %q has not enabled mcpUiAppTools", r, originOf[r])
			continue
		}
		if _, ok := grantedSet[r]; !ok {
			out[r] = fmt.Sprintf("%q is not in the deployment grant on AgentClass", r)
		}
	}
	return out
}

// UnrequestedActionTools returns, sorted and deduplicated, every entry of
// actionTools absent from requested — an AgentUI's own spec.tools. normalize
// (nil ⇒ identity) applies to both sides, because spec.tools and an action's
// Tool are independently-written strings and only the normalized form is a
// Loop.AppTools key.
//
// NOT a grant question: it does not consult the deployment grant. Materialize
// iterates `requested`, so a tool absent from it can never enter Loop.AppTools
// and every click on a control naming it answers AppToolCallStatusNotFound,
// however generously an AgentClass granted it. A tool neither requested nor
// granted is reported here too — one actionable message beats two.
func UnrequestedActionTools(requested, actionTools []string, normalize func(string) string) []string {
	if normalize == nil {
		normalize = func(s string) string { return s }
	}
	requestedSet := make(map[string]struct{}, len(requested))
	for _, r := range requested {
		requestedSet[normalize(r)] = struct{}{}
	}

	var out []string
	seen := map[string]struct{}{}
	for _, t := range actionTools {
		n := normalize(t)
		if _, ok := requestedSet[n]; ok {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// appVisible collects the tools of every opted-in origin, with an opted-out
// origin VETOING a name rather than losing a union to it. When two origins
// claim one tool name and only one enabled mcpUiAppTools, denial is the safe
// outcome: nothing here knows which origin's tool a call reaching that name
// would execute, so letting the enabled one "rescue" the name would silently
// override an unrelated origin's explicit opt-out. Veto is order-independent.
func appVisible(origins []Origin) map[string]struct{} {
	out := map[string]struct{}{}
	vetoed := map[string]struct{}{}
	for _, o := range origins {
		for _, t := range o.AppVisibleTools {
			if !o.AppToolsEnabled {
				vetoed[t] = struct{}{}
				continue
			}
			out[t] = struct{}{}
		}
	}
	for t := range vetoed {
		delete(out, t)
	}
	return out
}
