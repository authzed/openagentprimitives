package runner

import (
	"context"
	"log/slog"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/uigrant"
)

// AppToolsLogContext carries the identifying fields MaterializeAppTools logs
// when it denies part or all of a request — the same session/agentClass/
// agentUI keys a caller's own AgentUI-resolution error log already uses.
// AgentUI may be empty when the AgentClass carries no AgentUI grant at all.
type AppToolsLogContext struct {
	Session    string
	AgentClass string
	AgentUI    string
}

// ResolveAgentUITools resolves the AgentUI referenced by grant and returns
// its Spec.Tools — condition (1) of the three-way browser-tool grant (see
// MaterializeAppTools / pkg/web/uigrant.Materialize). A nil grant is not an
// error: (nil, nil) means the AgentClass carries no AgentUI at all, so no Get
// is attempted. Any Get error is returned so the caller can log it with its
// own session/agentClass context — MaterializeAppTools's fail-closed handling
// of a nil requested slice empties the browser-callable surface either way,
// but the specific reason still needs to reach a log, and only the caller
// knows its own session identity.
//
// internal/cmd/runner/main.go and test/e2e's in-process harness both call this, so
// AgentUI resolution cannot silently drift between production and the harness
// that is supposed to catch a regression in it.
func ResolveAgentUITools(ctx context.Context, c client.Client, namespace string, grant *spiceboxv1alpha1.AgentClassUIGrant) ([]string, error) {
	if grant == nil {
		return nil, nil
	}
	var aui spiceboxv1alpha1.AgentUI
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: grant.Ref}, &aui); err != nil {
		return nil, err
	}
	return aui.Spec.Tools, nil
}

// AppToolOrigin builds one origin's uigrant.Origin — condition (2) of the
// three-way browser-tool grant MaterializeAppTools enforces. name is the
// origin CR's bare metadata.name (NOT an LLM-prefix override); enabled mirrors
// mcpUiAppTools.enabled. appTools MUST be the already-synthesized,
// LLM-prefixed tool.Tools this origin produced
// (mcpdispatch.SynthesizeResult.AppTools): AppVisibleTools is built from each
// t.Name() (the "<ref>_<tool>" llmName), the same vocabulary
// AgentUI.spec.tools and AgentClass.spec.agentUI.grantedTools use. Bare
// upstream names would make uigrant.Materialize match zero keys in
// Loop.AppTools — fail closed, but silently, which pkg/web/uigrant's doc
// comment calls worse than a loud rejection.
func AppToolOrigin(name string, enabled bool, appTools []tool.Tool) uigrant.Origin {
	names := make([]string, 0, len(appTools))
	for _, t := range appTools {
		names = append(names, t.Name())
	}
	return uigrant.Origin{Name: name, AppToolsEnabled: enabled, AppVisibleTools: names}
}

// MaterializeAppTools is the SINGLE choke point turning accumulated MCP-UI
// app-visible-only tools into what a browser may actually call: the fail-closed
// three-way intersection pkg/web/uigrant.Materialize computes from requested
// (condition 1 — nil when no AgentUI could be resolved), origins (condition 2 —
// see AppToolOrigin), and grant.GrantedTools (condition 3 — grant is nil when
// the AgentClass carries no AgentUI grant at all). internal/cmd/runner/main.go and
// test/e2e's in-process harness both call this exact function so the harness's
// enforcement cannot silently drift from production's.
//
// requested and granted are normalized through synthesize.NormalizeName before
// the intersection, because Loop.AppTools keys are
// NormalizeName("<ref>_<tool>") — lowercased, anything outside [a-z0-9_-]
// replaced with '-', truncated at 128 (pkg/agent/tool/synthesize.Build).
// AgentUI.spec.tools and AgentClassUIGrant.grantedTools carry a CRD pattern
// equal to that alphabet, but MCPServerTool.Name carries none, and a CRD
// pattern is not retroactive: an object persisted before it shipped keeps
// non-conforming names until rewritten. Without normalizing, an author writing
// "widgets_createIssue" against a registry keyed "widgets_createissue" gets a
// silently empty AppTools while AgentUI.status.eligibleTools (uigrant.Ceiling
// over the same raw strings) reports the tool eligible, with nothing explaining
// the gap. origins is NOT normalized: AppToolOrigin builds it from each tool's
// real t.Name(), already the registry key. Two authored names normalizing to
// one key is fine — they would collide in the synthesized registry too.
//
// A narrower-than-requested intersection logs uigrant.Explain's per-tool
// diagnosis at Info, so the denial is never silent. Callers MUST assign this
// result (via appToolsByName) directly to Loop.AppTools and nowhere else: a
// tool missing here is simply not registered, so HandleAppToolCall's
// fail-closed lookup rejects a call to it, and there is no separate gate to
// keep in sync.
func MaterializeAppTools(logger *slog.Logger, logCtx AppToolsLogContext, requested []string, origins []uigrant.Origin, grant *spiceboxv1alpha1.AgentClassUIGrant, mcpAppTools []tool.Tool) []tool.Tool {
	if logger == nil {
		logger = slog.Default()
	}
	var grantedTools []string
	if grant != nil {
		grantedTools = grant.GrantedTools
	}
	normRequested := normalizeToolNames(requested)
	normGranted := normalizeToolNames(grantedTools)
	allowed := uigrant.Materialize(normRequested, origins, normGranted)
	if len(allowed) < len(normRequested) {
		logger.Info("browser-tool grant denied one or more requested app tools",
			"session", logCtx.Session,
			"agentClass", logCtx.AgentClass,
			"agentUI", logCtx.AgentUI,
			"explain", uigrant.Explain(normRequested, origins, normGranted))
	}
	if len(allowed) == 0 {
		return nil
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, n := range allowed {
		allowedSet[n] = struct{}{}
	}
	var out []tool.Tool
	for _, t := range mcpAppTools {
		if _, ok := allowedSet[t.Name()]; ok {
			out = append(out, t)
		}
	}
	return out
}

// normalizeToolNames applies synthesize.NormalizeName to every entry and
// dedupes on the normalized form (order-preserving on first occurrence).
// Matching uigrant.Materialize's own dedup semantics here is what lets
// MaterializeAppTools compare len(allowed) against len(normRequested) to
// detect a real denial — an un-deduped duplicate would otherwise look exactly
// like a tool the grant denied.
func normalizeToolNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		norm := synthesize.NormalizeName(n)
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	return out
}
