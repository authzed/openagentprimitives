package browser

import (
	"encoding/json"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/originfmt"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

// opSpec describes one observation tool's LLM-facing surface.
type opSpec struct {
	op          string
	description string
	schema      string
}

// observationOps is the Phase A observation tool set. screenshot is appended
// conditionally (acceptsImages); every other op is always offered.
var observationOps = []opSpec{
	{
		op:          "navigate",
		description: "Open an http(s) URL in the browser and return the final URL and page title. The URL's host must be permitted by the browser toolbox's egress policy.",
		schema:      `{"type":"object","properties":{"url":{"type":"string","description":"The absolute http(s) URL to open."}},"required":["url"]}`,
	},
	{
		op:          "read",
		description: "Render the current page as an indented accessibility outline with per-snapshot refs ([eN]). Paginated at 16 KiB; pass page to move through pages, or ref to scope to a subtree.",
		schema:      `{"type":"object","properties":{"page":{"type":"integer","description":"1-based page number (default 1)."},"ref":{"type":"string","description":"A ref from a prior read to scope the outline to that element's subtree."}}}`,
	},
	{
		op:          "find",
		description: "Find elements whose accessible name or text contains the given string (case-insensitive). Returns up to 50 matching [eN] lines.",
		schema:      `{"type":"object","properties":{"text":{"type":"string","description":"Substring to search for."}},"required":["text"]}`,
	},
	{
		op:          "follow",
		description: "Follow a link referenced by a ref from a prior read, navigating to its URL through the same egress policy as navigate. Fails if the ref is not a link or is stale.",
		schema:      `{"type":"object","properties":{"ref":{"type":"string","description":"A link ref (e.g. e3) from a prior read."}},"required":["ref"]}`,
	},
	{
		op:          "scroll",
		description: "Scroll the viewport up or down, or scroll a referenced element into view.",
		schema:      `{"type":"object","properties":{"direction":{"type":"string","enum":["up","down"],"description":"Scroll direction for the viewport."},"ref":{"type":"string","description":"A ref to scroll into view instead of the viewport."}}}`,
	},
	{
		op:          "back",
		description: "Go back one entry in the browser history; re-checks the landed host against the egress policy.",
		schema:      `{"type":"object","properties":{}}`,
	},
	{
		op:          "wait_for",
		description: "Poll the page until text appears (case-insensitive) or a referenced element is present, or until the timeout. The timeout is clamped to [1,30] seconds.",
		schema:      `{"type":"object","properties":{"text":{"type":"string","description":"Text to wait for."},"ref":{"type":"string","description":"A ref to wait for."},"timeout_seconds":{"type":"integer","description":"Seconds to wait; clamped to [1,30]."}}}`,
	},
}

// screenshotOp is offered only when the model accepts images.
var screenshotOp = opSpec{
	op:          "screenshot",
	description: "Capture a PNG screenshot of the viewport, or of a referenced element's bounding box.",
	schema:      `{"type":"object","properties":{"ref":{"type":"string","description":"A ref to clip the screenshot to."}}}`,
}

// Synthesize returns the observation tools for one resolved toolbox.
// gateURL is "http://<BrowserPodIP>:<GatePort>"; token is the per-session
// browser token; acceptsImages gates the screenshot tool (spec §3.2). All
// returned tools share one chromedp session (lazily connected on first use) and
// one origin.
func Synthesize(rt spiceboxv1alpha1.ResolvedBrowserToolbox, gateURL, token string, acceptsImages bool) ([]agenttool.Tool, error) {
	sites := make([]browserpolicy.Site, 0, len(rt.EffectiveSites))
	for _, s := range rt.EffectiveSites {
		sites = append(sites, browserpolicy.Site{Host: s.Host, Exact: s.Exact})
	}

	sess := newSession(gateURL, token)
	origin := originfmt.ForBrowser(rt)

	ops := make([]opSpec, len(observationOps))
	copy(ops, observationOps)
	if acceptsImages {
		ops = append(ops, screenshotOp)
	}

	// Phase A offers the identical observation set regardless of Interaction.
	// Acting tools (click/type/etc.) arrive in Phase C; the act branch exists
	// so the shape is explicit, but today it returns the same set.
	if rt.Spec.Interaction == "act" {
		// Phase C: append acting tools here. For now, same observation set.
		_ = rt.Spec.Interaction
	}

	tools := make([]agenttool.Tool, 0, len(ops))
	for _, o := range ops {
		tools = append(tools, &browserTool{
			name:        synthesize.FullName(rt.Name, o.op),
			op:          o.op,
			description: o.description,
			schema:      json.RawMessage(o.schema),
			origin:      origin,
			sess:        sess,
			rtName:      rt.Name,
			mode:        rt.Spec.Access.Mode,
			sites:       sites,
		})
	}
	return tools, nil
}
