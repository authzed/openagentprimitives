package runner

import (
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// ToolRefreshResult is what one mid-session refresh pass discovered. It carries
// both directions of "what changed about my tools since last turn?" — tools that
// arrived and tools that never will — because both come from one source (the
// session's resolved sidecars) and must not need two separate polls of it.
type ToolRefreshResult struct {
	// Added are tools that became available (or were re-synthesized against a
	// replaced pod) since the last pass, merged with replace-by-name semantics.
	Added []tool.Tool

	// Advisories are transient, one-per-occurrence `[system]` lines to inject
	// into THIS turn's context — e.g. a sidecar whose pod is wedged in a crash
	// loop, so the agent can tell the user its toolset is degraded and why
	// instead of silently proceeding without it. They are NOT persisted to the
	// transcript: they describe the state of this turn, and replaying them into
	// a later turn would re-assert a failure that may since have healed.
	Advisories []string
}

// applyToolRefresh merges refresher-returned tools into the live tool set with
// REPLACE-by-name semantics: an existing tool whose Name() collides with an
// `added` tool is dropped, then all `added` tools are appended. Duplicate names
// would confuse the LLM (buildToolDefs emits one def per slice entry) and
// dispatchToolUses' byName map would silently keep only one of the collisions.
//
// The replacement case is a secret-gated sidecar pod replaced mid-session: it
// re-emits the same `<ref>_<tool>` names against a new pod IP, so the stale
// tools must give way. Surviving tools keep their relative order; replacements
// are appended after them.
func applyToolRefresh(existing, added []tool.Tool) []tool.Tool {
	if len(added) == 0 {
		return existing
	}
	// A META tool is never something a refresher DISCOVERED, so it is never
	// something a refresher may replace.
	//
	// Replacement-by-name is right for the case it was built for: a
	// secret-gated sidecar re-probed after a token rotation re-emits the same
	// `<ref>_<tool>` names against a new pod IP, and leaving the stale entries
	// would make dispatch silently shadow one. But the names it could replace
	// were unbounded, and a SidecarToolbox ref is AUTHOR-CHOSEN — a ref named
	// `respond` exposing a tool named `to_user` emits `respond_to_user`, and the
	// refresh would hand the agent's terminal meta tool to an MCP server.
	// Everything downstream keys on that name.
	//
	// The refused entry is DROPPED rather than appended alongside: two tools of
	// one name is the shadowing this exists to prevent, just moved into the
	// dispatch map instead of the tool list.
	meta := make(map[string]struct{})
	for _, t := range existing {
		if t.Kind() == tool.KindMeta {
			meta[t.Name()] = struct{}{}
		}
	}
	kept := make([]tool.Tool, 0, len(added))
	for _, t := range added {
		if _, isMeta := meta[t.Name()]; isMeta {
			slog.Default().Info("mid-session tool refresh: refusing a tool that would shadow a meta tool",
				"tool", t.Name(), "kind", string(t.Kind()))
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == 0 {
		return existing
	}
	addedNames := make(map[string]struct{}, len(kept))
	for _, t := range kept {
		addedNames[t.Name()] = struct{}{}
	}
	out := make([]tool.Tool, 0, len(existing)+len(kept))
	for _, t := range existing {
		if _, replaced := addedNames[t.Name()]; replaced {
			continue // dropped; the `kept` slice carries its replacement
		}
		out = append(out, t)
	}
	return append(out, kept...)
}

// buildToolDefs builds the llm.ToolDef list sent to the provider from the
// current l.Tools plus l.ExtraToolDefs. Rebuilt each turn (cheap) so tools
// added mid-session by ToolRefresher reach the LLM on the next turn.
//
// Anthropic caps the request at 4 cache_control breakpoints, so only the last
// client-side tool is marked: one breakpoint covers every preceding tool def.
// When ToolRefresher appends tools the marker moves to the new last tool — the
// prefix is unchanged, so the cache up to the previous boundary still hits.
func (l *Loop) buildToolDefs() []llm.ToolDef {
	return buildToolDefsFrom(l.Tools, l.ExtraToolDefs)
}

// buildToolDefsFrom is buildToolDefs over an EXPLICIT tool set, so a caller can
// build the defs for a subset of l.Tools without mutating the live set. The
// cache breakpoint follows the subset's own last client-side tool.
func buildToolDefsFrom(tools []tool.Tool, extra []llm.ToolDef) []llm.ToolDef {
	toolDefs := make([]llm.ToolDef, 0, len(tools)+len(extra))
	for i, tt := range tools {
		toolDefs = append(toolDefs, llm.ToolDef{
			Name:        tt.Name(),
			Description: tt.Description(),
			InputSchema: tt.InputSchema(),
			Cacheable:   i == len(tools)-1,
		})
	}
	// Append provider-supplied server-tool defs (e.g. Anthropic
	// web_search_20250305 / web_fetch_20250910). These are dispatched by
	// the LLM API inline, not by the runner.
	toolDefs = append(toolDefs, extra...)
	return toolDefs
}

// replayToolDefs narrows one request's tool list to what a whole-session REPLAY
// says the captured run was offered at this turn.
//
// nil ReplayToolCatalog — production, and every scenario that pins no catalog —
// never reaches here; the caller passes defs straight through. See that field's
// doc for why the seam exists at all.
//
// The seam is asked about the WHOLE def list, provider server-tools included,
// because that is the list the tool_catalog Kind records; but only l.Tools can
// actually be narrowed, so a server-tool the seam declines stays offered. That
// is honest rather than silent: the seam has already reported it.
func (l *Loop) replayToolDefs(turnIndex int, defs []llm.ToolDef) []llm.ToolDef {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	keep := make(map[string]bool)
	for _, n := range l.ReplayToolCatalog(turnIndex, names) {
		keep[n] = true
	}
	tools := make([]tool.Tool, 0, len(l.Tools))
	for _, tt := range l.Tools {
		if keep[tt.Name()] {
			tools = append(tools, tt)
		}
	}
	return buildToolDefsFrom(tools, l.ExtraToolDefs)
}

// markCacheBreakpoints sets the two conversation cache breakpoints this repo's
// budget allows: an anchor at the previous request's breakpoint position, and a
// moving breakpoint at the end of the current messages.
//
// The moving breakpoint extends cache coverage as the conversation grows. The
// anchor is the fallback: a provider breakpoint walks back only 20 content
// blocks to find a prior entry, so one wide fan-out of parallel tool calls can
// push that entry out of reach — without the anchor, such a request re-processes
// the whole conversation at full price.
//
// Callers must not set ContentBlock.Cacheable anywhere else — the two
// breakpoints here plus the system block and last tool definition exactly
// exhaust the four-breakpoint budget.
func (l *Loop) markCacheBreakpoints(msgs []llm.Message) {
	if len(msgs) == 0 {
		return
	}
	// Clear any marks from a previous call so repeated marking of the same
	// slice cannot accumulate past the budget.
	for i := range msgs {
		for j := range msgs[i].Content {
			msgs[i].Content[j].Cacheable = false
		}
	}

	markLastBlock := func(i int) {
		if i < 0 || i >= len(msgs) {
			return
		}
		if n := len(msgs[i].Content); n > 0 {
			msgs[i].Content[n-1].Cacheable = true
		}
	}

	moving := len(msgs) - 1
	// A stale anchor (history compacted, forked, or replayed shorter) is
	// dropped rather than clamped: pointing it at an arbitrary surviving
	// message would write a fresh entry, not reuse one.
	if l.hasPrevBreakpoint && l.prevBreakpointMsg < moving {
		markLastBlock(l.prevBreakpointMsg)
	}
	markLastBlock(moving)
	l.prevBreakpointMsg = moving
	l.hasPrevBreakpoint = true
}
