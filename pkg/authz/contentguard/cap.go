// The framework-side byte cap on what any Instance is handed to inspect. It is
// a decorator over Instance rather than a call-site check so the two inspecting
// paths — the pipeline adapter for gated tools, and the runner's meta-tool
// inspection — cap identically, and no third caller can opt out by reaching for
// Inspect directly.
package contentguard

import (
	"context"
	"unicode/utf8"
)

// MaxInspectBytes caps how much of a tool call's content — its result at
// PostToolCall, its serialized arguments at PreToolCall — is handed in one
// Inspect call to a content inspector that a large payload could outrun. An
// inspector declaring WholeContentInspector is exempt; the reasoning below is
// the question that declaration answers.
//
// Without a cap the BIGGEST payloads are the LEAST likely to be scanned, and
// nothing else bounds one before inspection. toolguard's byte budgets
// (maxIngressBytes / maxEgressBytes) are unset by default and, when set, DENY an
// oversized payload rather than trimming it; query_memory's limit clamps entries
// (100), not bytes. Meanwhile the prompt-injection inspector times its detector
// POST out after ~1s and, under its default onError=warn, turns that timeout
// into a Pass. Uncapped, a megabyte payload therefore sails through unscanned
// while a small one carrying the same injection is blocked — a knob a hostile
// MCP or sidecar server reaches for by padding its output, and one an injected
// model reaches for by padding respond_to_user's args.
//
// 32 KiB is chosen against the DETECTOR, not the payload: a local ONNX text
// classifier with a ~512-token (≈2 KiB) window, so 32 KiB is already ~16× more
// than it can attend to and no realistic coverage is lost, while a 32 KiB
// localhost round trip stays well inside the budget. Deliberately NOT windowed:
// the classifier truncates internally anyway, so chunking buys no detection and
// costs N sequential round trips of turn latency. Truncation is never silent —
// it lands on the Finding's Details (content_bytes / inspected_bytes), which
// every audit sink forwards.
const MaxInspectBytes = 32 << 10

// Capped wraps inst so every Inspect sees at most MaxInspectBytes of the
// subject's content — unless inst declares WholeContentInspector, below — and
// so any truncation is stamped onto the returned Finding's Details for the
// caller's audit record.
//
// BOTH sides are bounded, by different mechanics. The result is trimmed on the
// Subject itself, so even an inspector reading Subject.Result directly is
// bounded. The args are bounded via Subject.Text instead, because Subject.Args
// is a json.RawMessage and slicing it would hand a structured inspector invalid
// JSON — the full args stay parseable while the TEXT an inspector scans is
// capped. The args need the cap as much as the result: they are authored by the
// model, which is exactly whom a prompt-injection inspector polices once that
// model has been injected.
//
// The cap follows the INSPECTOR, not the content's origin. An inst declaring
// WholeContentInspector is handed everything, because it cannot be outrun and a
// cap on it is purely a place for an attacker to hide payload; everything else
// takes the cap. This wrapper is the ONLY grantor of the exemption, so an
// Instance invoked without it stays bounded regardless.
//
// Idempotent: wrapping an already-capped Instance returns it unchanged, so the
// independent wrap sites may each apply it without double-truncating or
// double-stamping.
func Capped(inst Instance) Instance {
	if inst == nil {
		return nil
	}
	if c, ok := inst.(*capped); ok {
		return c
	}
	return &capped{Instance: inst}
}

type capped struct{ Instance }

func (c *capped) Inspect(ctx context.Context, s Subject) (Finding, error) {
	// rawText/Text cover whichever side this point carries, so the counters are
	// stamped for a capped args subject exactly as for a capped result one.
	full := len(s.rawText())
	if inspectsWholeContent(c.Instance) {
		// This inspector cannot be outrun, so the cap would only hand an
		// attacker a place to hide (see WholeContentInspector). Result is left
		// untrimmed for the same reason: the trim in the other branch exists to
		// bound an inspector reading Subject.Result directly, and that is this
		// same inspector.
		s.wholeContent = true
	} else {
		s.Result = capForInspection(s.Result)
	}
	submitted := len(s.Text())

	f, err := c.Instance.Inspect(ctx, s)
	if err != nil {
		// The adapter and the runner both fail CLOSED on an inspect error, so
		// nothing reached the model unscanned and there is no coverage gap to
		// record. The caller stamps the error onto its own audit event.
		return f, err
	}
	f.Details = withInspectedBytes(f.Details, full, submitted)
	return f, nil
}

// capForInspection returns at most MaxInspectBytes of s, trimmed back to a
// whole rune so a detector is never handed a half-decoded code point.
func capForInspection(s string) string {
	if len(s) <= MaxInspectBytes {
		return s
	}
	n := MaxInspectBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// withInspectedBytes copies details and, when the submitted content was capped
// at MaxInspectBytes, stamps how much of it the inspector actually saw. The
// truncation is a real gap in coverage, so it lands in the durable audit entry
// rather than being silent (AGENTS.md: never silently drop). Copies rather than
// mutating because details is usually the inspector's own Finding.Details map.
func withInspectedBytes(details map[string]any, contentBytes, inspectedBytes int) map[string]any {
	if inspectedBytes >= contentBytes {
		return details
	}
	out := make(map[string]any, len(details)+2)
	for k, v := range details {
		out[k] = v
	}
	out["content_bytes"] = contentBytes
	out["inspected_bytes"] = inspectedBytes
	return out
}
