package browser

import (
	"encoding/json"
	"fmt"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
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

// actingOps are the Phase C acting tools (spec §3.2), offered ONLY when the
// toolbox's Interaction is "act". Each reaches and mutates the live page, so each
// authorizes External on the web_host slot for the current page host (see
// browserTool.Permission).
var actingOps = []opSpec{
	{
		op: "type",
		description: "Type text into the input named by a ref from a prior read (focuses it first). " +
			"text may embed a stored login WITHOUT you ever seeing its value, using placeholders: " +
			"{{login.<host>.username}}, {{login.<host>.password}}, {{login.<host>.otp}} — where <host> is " +
			"a login's host from query_logins and must be the host of the page you are currently on. " +
			"A {{login...password}} placeholder may only be typed into a password field. Refs from before " +
			"a type remain valid for the next field, so you can fill a username, password and submit from one read.",
		schema: `{"type":"object","properties":{"ref":{"type":"string","description":"A ref (e.g. e3) from a prior read identifying the input to type into."},"text":{"type":"string","description":"The text to type. May contain {{login.<host>.username|password|otp}} placeholders resolved against the current page's stored login."}},"required":["ref","text"]}`,
	},
	{
		op:          "click",
		description: "Click the element named by a ref from a prior read (a button, link, checkbox, etc.), firing its handlers and any form submission. Acts on the live page; refs from before the click are stale afterward — read again.",
		schema:      `{"type":"object","properties":{"ref":{"type":"string","description":"A ref (e.g. e3) from a prior read identifying the element to click."}},"required":["ref"]}`,
	},
	{
		op:          "press",
		description: "Press one key on the page. Use a single character to type it, or a named key: Enter, Tab, Escape, Backspace, Delete, ArrowUp, ArrowDown, ArrowLeft, ArrowRight, Home, End, PageUp, PageDown, Space.",
		schema:      `{"type":"object","properties":{"key":{"type":"string","description":"A single character, or a named key such as Enter or Tab."}},"required":["key"]}`,
	},
	{
		op:          "select",
		description: "Choose an option in a <select> named by a ref from a prior read. option is matched against each choice's value, label and visible text; the change fires the page's handlers. Refs from before are stale afterward.",
		schema:      `{"type":"object","properties":{"ref":{"type":"string","description":"A ref (e.g. e3) from a prior read identifying the <select>."},"option":{"type":"string","description":"The option to choose, by its value, label, or visible text."}},"required":["ref","option"]}`,
	},
	{
		op:          "submit",
		description: "Submit the form that the element named by a ref belongs to (its own form, or the nearest enclosing one), running the form's submit handler. Refs from before the submit are stale afterward.",
		schema:      `{"type":"object","properties":{"ref":{"type":"string","description":"A ref (e.g. e3) from a prior read for an element inside the form to submit."}},"required":["ref"]}`,
	},
	{
		op: requestHumanStepOp,
		description: "Ask a person to complete a sign-in step you can't do yourself; the result is a STATUS only and never contains a code or secret. " +
			"With kind=\"confirm\" it posts a card with a Done button and BLOCKS until the person confirms (use this for a push notification they approve on their phone, or any \"do X then press Done\" step). " +
			"With kind=\"code\" it asks the person for an emailed or texted code and, once they provide it, types it into the input you name with ref and BLOCKS until then — you never see the code, it is entered for you (read the page first to get the code field's ref). " +
			"Omit kind to report a CAPTCHA or any other step a person must take over — a notice is sent and the call reports that human intervention is needed (it does not wait). " +
			"The prompt should say plainly what the person needs to do.",
		schema: `{"type":"object","properties":{"kind":{"type":"string","enum":["confirm","code"],"description":"\"confirm\" posts a Done button and blocks until a person confirms. \"code\" asks for an emailed/SMS code and types it into ref for you. Omit kind entirely to report a CAPTCHA or other step needing a person."},"prompt":{"type":"string","description":"A short message telling the person what to do (e.g. \"approve the push notification on your phone\", or \"enter the 6-digit code we emailed you\")."},"ref":{"type":"string","description":"Required for kind=\"code\": a ref (e.g. e3) from a prior read naming the input the code should be typed into."}},"required":["prompt"]}`,
	},
}

// screenshotOp is offered only when the model accepts images.
var screenshotOp = opSpec{
	op:          "screenshot",
	description: "Capture a PNG screenshot of the viewport, or of a referenced element's bounding box.",
	schema:      `{"type":"object","properties":{"ref":{"type":"string","description":"A ref to clip the screenshot to."}}}`,
}

// queryLoginsOp is offered only when the toolbox binds an identity (spec
// §3.2) — in BOTH act and readonly modes, independent of the
// acceptsImages/Interaction gates above. It is the one tool that reads the
// identity's login catalog and the toolbox's egress policy together: a model
// planning a login finds which hosts it can sign into, and separately checks
// whether an arbitrary host is in policy, without a round-trip through
// navigate just to find out.
var queryLoginsOp = opSpec{
	op:          "query_logins",
	description: "List the bound identity's stored logins. filter (optional) is matched case-insensitively against each login's hosts and label; omit it to list everything. Each result is {hosts, label} — nothing else about the login is exposed. Paginated. Pass host (optional) to additionally learn whether that hostname is within this toolbox's egress policy, independent of filter.",
	schema:      `{"type":"object","properties":{"filter":{"type":"string","description":"Case-insensitive substring matched against each login's hosts and label."},"host":{"type":"string","description":"A hostname to check against the toolbox's egress policy; the result states whether it is allowed."},"page":{"type":"integer","description":"1-based page number (default 1)."}}}`,
}

// Synthesize returns the observation tools for one resolved toolbox.
// gateURL is "http://<BrowserPodIP>:<GatePort>"; token is the per-session
// browser token; acceptsImages gates the screenshot tool (spec §3.2). fetch is
// the credential-filler's Fetch seam: the production runner builds it from the
// bound identity's directcred source (so a {{login...}} placeholder resolves to
// a real website login at use time), tests and the e2e harness pass a fake, and
// a toolbox with no bound identity passes nil (its catalog is empty, so no
// placeholder can resolve anyway). humanStep backs request_human_step's blocking
// confirm + captcha notice (offered only in act mode); the production runner
// passes a late-bound requester over its approval host, tests pass a fake, and a
// nil requester leaves request_human_step offered but erroring at call time.
// store persists cookies + localStorage across reap/wake for a `persistent`
// toolbox (Task C9): the production runner passes a StateStore over the
// per-session state Secret, tests pass a fake, and an `ephemeral` toolbox (or any
// caller that does not persist) passes nil, which disables capture and inject
// entirely. cj is the consequence judge (Phase E's E8 gate): the production
// runner passes E7's Haiku-backed judge (falling back to the deterministic one),
// tests pass a fake, and nil disables per-action (web_action) classification —
// the tool then stamps no web_action args and only the coarse web_host gate
// applies. All returned tools share one chromedp session (lazily connected on
// first use), one origin, one credential filler, one human-step requester, one
// state store, and one consequence judge.
func Synthesize(rt spiceboxv1alpha1.ResolvedBrowserToolbox, gateURL, token string, acceptsImages bool, fetch CredentialFetcher, humanStep HumanStepRequester, store StateStore, cj judge.ConsequenceJudge) ([]agenttool.Tool, error) {
	sites := make([]browserpolicy.Site, 0, len(rt.EffectiveSites))
	for _, s := range rt.EffectiveSites {
		sites = append(sites, browserpolicy.Site{Host: s.Host, Exact: s.Exact})
	}

	sess := newSession(gateURL, token)
	// Persistent-state wiring (Task C9): a non-nil store enables capture-after-act,
	// capture-on-stop, and inject-on-first-attach. nil (ephemeral) leaves them off.
	sess.store = store
	sess.logName = rt.Name
	origin := originfmt.ForBrowser(rt)
	// One filler per toolbox, sharing the session's §5.3 hold. Built here, after
	// the session, because the default filler reaches the hold through it.
	filler := newCredFiller(sess, rt.Catalog, fetch)

	ops := make([]opSpec, len(observationOps))
	copy(ops, observationOps)
	if acceptsImages {
		ops = append(ops, screenshotOp)
	}
	// Offered iff the toolbox binds an identity — independent of
	// acceptsImages and of the act/readonly Interaction gate below. A toolbox
	// with no identity has no catalog, so there is nothing for this tool to
	// find.
	if rt.IdentityName != "" {
		ops = append(ops, queryLoginsOp)
	}

	// Acting tools (click/press/select/submit) are offered ONLY in act mode;
	// readonly offers the observation set alone. Each acting tool authorizes
	// External on the web_host slot for the live page host (browserTool.Permission).
	if rt.Spec.Interaction == "act" {
		ops = append(ops, actingOps...)
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
			catalog:     rt.Catalog,
			filler:      filler,
			humanStep:   humanStep,
			judge:       cj,
		})
	}
	return tools, nil
}

// PromptSection returns the toolbox's one-line system-prompt fragment (spec
// §5.1), or nil when the toolbox binds no identity — mirroring
// queryLoginsOp's own offered-iff-identity gate, so the prompt never
// advertises a tool the tool list does not actually contain.
//
// The browser toolbox is hand-wired directly into internal/cmd/runner's tool
// list, not through the capability.Capability/SectionOfferer registry (see
// pkg/agent/tool/sidecartoolbox's doc comment for why execution-side toolbox
// synthesizers are wired this way); this still returns capability.PromptSection
// so the one caller renders it through the exact same "## Title\n\nBody"
// path every other capability-contributed section uses, and so a future move
// of the browser toolbox onto the capability registry costs nothing here.
//
// unattended is part of the catalog shape the spec table describes
// (§3.2), but it is a property of a SITE (BrowserSite.Unattended, added by
// Phase C Task C8), not of a BrowserLoginRef — and BrowserEffectiveSite
// carries no Unattended field yet either, so there is no data to derive a
// per-login unattended flag from today. This fragment states counts only and
// omits any mention of unattended; once C8 threads Unattended onto
// BrowserEffectiveSite, this is the place to add the per-entry/per-host
// derivation.
func PromptSection(rt spiceboxv1alpha1.ResolvedBrowserToolbox) *capability.PromptSection {
	if rt.IdentityName == "" {
		return nil
	}
	n := len(rt.Catalog)
	logins := "logins"
	if n == 1 {
		logins = "login"
	}
	line := fmt.Sprintf("%s has %d stored %s; query_logins finds them by host or label.", rt.Name, n, logins)
	if rt.Spec.Access.Mode == browserpolicy.ModeAllowlist {
		h := len(rt.EffectiveSites)
		hosts, verb := "hosts", "are"
		if h == 1 {
			hosts, verb = "host", "is"
		}
		line += fmt.Sprintf(" In allowlist mode, %d %s %s allowed; query_logins also answers whether a given host is one of them.", h, hosts, verb)
	}
	return &capability.PromptSection{
		Title: fmt.Sprintf("Browser toolbox: %s", rt.Name),
		Body:  line,
	}
}
