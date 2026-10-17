package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

const (
	// maxPageBytes is the read pagination budget per page.
	maxPageBytes = 16 * 1024
	// navigateTimeout bounds a navigation (navigate / follow / back).
	navigateTimeout = 30 * time.Second
	// snapshotTimeout bounds an accessibility snapshot (read / find / screenshot).
	snapshotTimeout = 20 * time.Second
	// shortTimeout bounds a quick action (scroll, a ref resolve).
	shortTimeout = 10 * time.Second
	// pollInterval is how often wait_for re-snapshots.
	pollInterval = 250 * time.Millisecond

	waitMinTimeout     = 1 * time.Second
	waitMaxTimeout     = 30 * time.Second
	waitDefaultTimeout = 30 * time.Second
)

// browserTool is one observation tool for a resolved BrowserToolbox. Every tool
// of a toolbox shares one *Session and one origin; op selects the behavior.
type browserTool struct {
	name        string
	op          string
	description string
	schema      json.RawMessage
	origin      string

	sess   *Session
	rtName string // the LLM-facing toolbox Name, embedded in the policy error text
	mode   string
	sites  []browserpolicy.Site
}

var (
	_ agenttool.Tool        = (*browserTool)(nil)
	_ agenttool.OriginTool  = (*browserTool)(nil)
	_ agenttool.Cancellable = (*browserTool)(nil)
)

func (t *browserTool) Name() string                 { return t.name }
func (t *browserTool) Kind() agenttool.Kind         { return agenttool.KindMCP }
func (t *browserTool) Description() string          { return t.description }
func (t *browserTool) InputSchema() json.RawMessage { return t.schema }

// Permission is Passthrough: a browser observation reaches external state but
// maps to no SpiceDB resource, so no per-tool Check is required — the gate's
// egress policy, not SpiceDB, authorizes which hosts the agent may reach.
func (t *browserTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (t *browserTool) PermissionVariants() []authz.PermissionVariant { return nil }

// Origin is shared across one toolbox's tools so toolguard's origin-level
// circuit breaker treats them as a single upstream.
func (t *browserTool) Origin() string { return t.origin }

// Cancel tears down the shared browser connection, interrupting any in-flight
// call and releasing its goroutines; the next tool call reconnects lazily.
func (t *browserTool) Cancel(context.Context) error {
	t.sess.Close()
	return nil
}

// Execute parses the tool's flat arguments and dispatches by op. Tool-level
// failures (bad scheme, policy refusal, stale ref) are returned as IsError
// results, never as a Go error — a Go error is reserved for a runner-side
// dispatch fault.
func (t *browserTool) Execute(ctx context.Context, raw json.RawMessage, _ *agenttool.SessionContext) (agenttool.Result, error) {
	var a struct {
		URL            string `json:"url"`
		Page           int    `json:"page"`
		Ref            string `json:"ref"`
		Text           string `json:"text"`
		Direction      string `json:"direction"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return errResult("invalid arguments: %v", err), nil
		}
	}
	switch t.op {
	case "navigate":
		if a.URL == "" {
			return errResult("navigate requires a url"), nil
		}
		return t.navigateTo(ctx, a.URL), nil
	case "read":
		return t.read(ctx, a.Page, a.Ref), nil
	case "find":
		return t.find(ctx, a.Text), nil
	case "screenshot":
		return t.screenshot(ctx, a.Ref), nil
	case "follow":
		return t.follow(ctx, a.Ref), nil
	case "scroll":
		return t.scroll(ctx, a.Direction, a.Ref), nil
	case "back":
		return t.back(ctx), nil
	case "wait_for":
		return t.waitFor(ctx, a.Text, a.Ref, a.TimeoutSeconds), nil
	default:
		return errResult("unknown browser tool %q", t.op), nil
	}
}

// decide evaluates host against the toolbox's egress policy.
func (t *browserTool) decide(host string) browserpolicy.Decision {
	return browserpolicy.Decide(t.mode, t.sites, host)
}

// policyError renders the verbatim policy-refusal contract text.
func (t *browserTool) policyError(host, rule string) agenttool.Result {
	return errResult("host %s is not allowed by browser toolbox %s (rule: %s)", host, t.rtName, rule)
}

// navigateTo pre-checks scheme + host policy BEFORE touching Chrome, then
// navigates. A proxy-denied load (a redirect to an off-policy host) is reported
// as the policy error rather than the Chrome error page.
func (t *browserTool) navigateTo(ctx context.Context, rawURL string) agenttool.Result {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errResult("only http and https URLs can be opened")
	}
	host := u.Hostname()
	if dec := t.decide(host); !dec.Allowed {
		return t.policyError(host, dec.Rule)
	}

	var out agenttool.Result
	runErr := t.sess.do(ctx, navigateTimeout, func(c context.Context) error {
		t.sess.resetBlocked()
		var navErrText string
		if e := runAction(c, func(ac context.Context) error {
			_, _, et, _, e := page.Navigate(rawURL).Do(ac)
			navErrText = et
			return e
		}); e != nil {
			return e
		}
		if navErrText != "" {
			if isProxyDenial(navErrText) {
				// A host in the (possibly redirected) chain was denied by the
				// gate's egress proxy. Name the real blocked host when the
				// network listener captured it; else fall back to the landed URL.
				bh := t.sess.blockedHost()
				if bh == "" {
					bh = t.landedHost(c, host)
				}
				rule := t.decide(bh).Rule
				if rule == "" {
					rule = "blocked by egress policy"
				}
				out = t.policyError(bh, rule)
				return nil
			}
			out = errResult("navigation failed: %s", navErrText)
			return nil
		}
		finalURL, title := t.locationTitle(c)
		t.sess.clearRefs()
		out = okResult(fmt.Sprintf("loaded %s — title: %s", finalURL, title))
		return nil
	})
	if runErr != nil {
		return errResult("navigation failed: %v", runErr)
	}
	return out
}

// landedHost best-effort reads the host Chrome ended on after a failed
// navigation, for naming the policy error; falls back to the requested host.
func (t *browserTool) landedHost(c context.Context, fallback string) string {
	var loc string
	_ = runAction(c, func(ac context.Context) error { return chromedp.Location(&loc).Do(ac) })
	if lu, e := url.Parse(loc); e == nil && lu.Hostname() != "" {
		return lu.Hostname()
	}
	return fallback
}

// locationTitle reads the current URL and title, ignoring individual errors
// (either may be empty on an odd page without failing the whole navigation).
func (t *browserTool) locationTitle(c context.Context) (string, string) {
	var loc, title string
	_ = runAction(c, func(ac context.Context) error {
		_ = chromedp.Location(&loc).Do(ac)
		_ = chromedp.Title(&title).Do(ac)
		return nil
	})
	return loc, title
}

func (t *browserTool) read(ctx context.Context, pageNum int, ref string) agenttool.Result {
	var out agenttool.Result
	runErr := t.sess.do(ctx, snapshotTimeout, func(c context.Context) error {
		var startBackend int64
		if ref != "" {
			e, ok := t.sess.lookupRef(ref)
			if !ok {
				out = errResult("ref %s is stale; call read again", ref)
				return nil
			}
			startBackend = e.backendNodeID
		}
		lines, refs, e := snapshot(c, startBackend)
		if e != nil {
			return e
		}
		t.sess.setRefs(refs)
		pages := paginate(renderOutline(lines), maxPageBytes)
		p := pageNum
		if p <= 0 {
			p = 1
		}
		if p > len(pages) {
			out = errResult("page %d is out of range", p)
			return nil
		}
		out = okResult(fmt.Sprintf("%s\n\npage %d of %d", pages[p-1], p, len(pages)))
		return nil
	})
	if runErr != nil {
		return errResult("read failed: %v", runErr)
	}
	return out
}

func (t *browserTool) find(ctx context.Context, text string) agenttool.Result {
	if strings.TrimSpace(text) == "" {
		return errResult("find requires non-empty text")
	}
	var out agenttool.Result
	runErr := t.sess.do(ctx, snapshotTimeout, func(c context.Context) error {
		lines, refs, e := snapshot(c, 0)
		if e != nil {
			return e
		}
		t.sess.setRefs(refs)
		needle := strings.ToLower(text)
		matched := make([]string, 0, 50)
		for _, ln := range lines {
			if strings.Contains(strings.ToLower(ln.name), needle) {
				matched = append(matched, fmt.Sprintf("[%s] %s %q", ln.ref, ln.role, ln.name))
				if len(matched) >= 50 {
					break
				}
			}
		}
		if len(matched) == 0 {
			out = okResult(fmt.Sprintf("no matches for %q", text))
			return nil
		}
		out = okResult(strings.Join(matched, "\n"))
		return nil
	})
	if runErr != nil {
		return errResult("find failed: %v", runErr)
	}
	return out
}

func (t *browserTool) follow(ctx context.Context, ref string) agenttool.Result {
	if ref == "" {
		return errResult("follow requires a ref")
	}
	var handled *agenttool.Result
	var href string
	runErr := t.sess.do(ctx, snapshotTimeout, func(c context.Context) error {
		e, ok := t.sess.lookupRef(ref)
		if !ok {
			r := errResult("ref %s is stale; call read again", ref)
			handled = &r
			return nil
		}
		if e.role != "link" {
			r := errResult("ref %s is not a link", ref)
			handled = &r
			return nil
		}
		var node *cdp.Node
		if ae := runAction(c, func(ac context.Context) error {
			var e2 error
			node, e2 = dom.DescribeNode().WithBackendNodeID(cdp.BackendNodeID(e.backendNodeID)).Do(ac)
			return e2
		}); ae != nil {
			return ae
		}
		base, _ := t.locationTitle(c)
		href = resolveHref(base, attrValue(node, "href"))
		return nil
	})
	if runErr != nil {
		return errResult("follow failed: %v", runErr)
	}
	if handled != nil {
		return *handled
	}
	if href == "" {
		return errResult("ref %s has no link target", ref)
	}
	return t.navigateTo(ctx, href)
}

func (t *browserTool) back(ctx context.Context) agenttool.Result {
	var out agenttool.Result
	runErr := t.sess.do(ctx, navigateTimeout, func(c context.Context) error {
		if e := runAction(c, func(ac context.Context) error { return chromedp.NavigateBack().Do(ac) }); e != nil {
			return e
		}
		finalURL, title := t.locationTitle(c)
		t.sess.clearRefs()
		if lu, e := url.Parse(finalURL); e == nil && lu.Hostname() != "" {
			if dec := t.decide(lu.Hostname()); !dec.Allowed {
				out = t.policyError(lu.Hostname(), dec.Rule)
				return nil
			}
		}
		out = okResult(fmt.Sprintf("loaded %s — title: %s", finalURL, title))
		return nil
	})
	if runErr != nil {
		return errResult("back failed: %v", runErr)
	}
	return out
}

func (t *browserTool) scroll(ctx context.Context, direction, ref string) agenttool.Result {
	if ref == "" && direction != "up" && direction != "down" {
		return errResult(`scroll requires direction "up" or "down" (or a ref to scroll into view)`)
	}
	var out agenttool.Result
	runErr := t.sess.do(ctx, shortTimeout, func(c context.Context) error {
		if ref != "" {
			e, ok := t.sess.lookupRef(ref)
			if !ok {
				out = errResult("ref %s is stale; call read again", ref)
				return nil
			}
			if ae := runAction(c, func(ac context.Context) error {
				return dom.ScrollIntoViewIfNeeded().WithBackendNodeID(cdp.BackendNodeID(e.backendNodeID)).Do(ac)
			}); ae != nil {
				return ae
			}
			out = okResult(fmt.Sprintf("scrolled ref %s into view", ref))
			return nil
		}
		delta := "window.innerHeight"
		if direction == "up" {
			delta = "-window.innerHeight"
		}
		var ignore int
		if ae := runAction(c, func(ac context.Context) error {
			return chromedp.Evaluate("window.scrollBy(0, "+delta+"); 0", &ignore).Do(ac)
		}); ae != nil {
			return ae
		}
		out = okResult("scrolled " + direction)
		return nil
	})
	if runErr != nil {
		return errResult("scroll failed: %v", runErr)
	}
	return out
}

// screenshot captures a PNG (validating ref/page state along the way — the
// box-model lookup above fails loudly on a stale ref) but does NOT return the
// image bytes to the model: agenttool.Result carries only text today, so a
// base64 data-URI would just be an unrenderable, budget-burning text blob. The
// capture is kept — it is still useful as a liveness/validity check — and the
// result is a short text stub instead. acceptsImages (synthesize.go) still
// gates whether this tool is offered at all; that plumbing is correct as-is
// and is exactly what should flip this stub over to real image delivery once
// agenttool.Result grows an image field.
func (t *browserTool) screenshot(ctx context.Context, ref string) agenttool.Result {
	var out agenttool.Result
	runErr := t.sess.do(ctx, snapshotTimeout, func(c context.Context) error {
		var data []byte
		if ref != "" {
			e, ok := t.sess.lookupRef(ref)
			if !ok {
				out = errResult("ref %s is stale; call read again", ref)
				return nil
			}
			var box *dom.BoxModel
			if ae := runAction(c, func(ac context.Context) error {
				var e2 error
				box, e2 = dom.GetBoxModel().WithBackendNodeID(cdp.BackendNodeID(e.backendNodeID)).Do(ac)
				return e2
			}); ae != nil {
				return ae
			}
			if ae := runAction(c, func(ac context.Context) error {
				var e2 error
				data, e2 = page.CaptureScreenshot().WithClip(clipFromBox(box)).Do(ac)
				return e2
			}); ae != nil {
				return ae
			}
		} else {
			if ae := runAction(c, func(ac context.Context) error {
				var e2 error
				data, e2 = page.CaptureScreenshot().Do(ac)
				return e2
			}); ae != nil {
				return ae
			}
		}
		out = okResult(fmt.Sprintf("screenshot captured (%d bytes PNG); image delivery to the model is not yet supported", len(data)))
		return nil
	})
	if runErr != nil {
		return errResult("screenshot failed: %v", runErr)
	}
	return out
}

func (t *browserTool) waitFor(ctx context.Context, text, ref string, seconds int) agenttool.Result {
	if strings.TrimSpace(text) == "" && ref == "" {
		return errResult("wait_for requires one of text or ref")
	}
	d, clamped := clampWaitTimeout(seconds)
	suffix := ""
	if clamped {
		suffix = " (timeout clamped to 30s)"
	}

	// Resolve a ref target once up front; a stale ref is refused immediately.
	var targetBackend int64
	if ref != "" {
		var staleRes *agenttool.Result
		if e := t.sess.do(ctx, shortTimeout, func(context.Context) error {
			entry, ok := t.sess.lookupRef(ref)
			if !ok {
				r := errResult("ref %s is stale; call read again", ref)
				staleRes = &r
				return nil
			}
			targetBackend = entry.backendNodeID
			return nil
		}); e != nil {
			return errResult("wait_for failed: %v", e)
		}
		if staleRes != nil {
			return *staleRes
		}
	}

	deadline := time.Now().Add(d)
	needle := strings.ToLower(text)
	for {
		var found bool
		if e := t.sess.do(ctx, snapshotTimeout, func(c context.Context) error {
			lines, refs, se := snapshot(c, 0)
			if se != nil {
				return se
			}
			t.sess.setRefs(refs)
			for _, ln := range lines {
				if text != "" && strings.Contains(strings.ToLower(ln.name), needle) {
					found = true
					break
				}
				if ref != "" && ln.backendNodeID == targetBackend {
					found = true
					break
				}
			}
			return nil
		}); e != nil {
			return errResult("wait_for failed: %v", e)
		}
		if found {
			return okResult(fmt.Sprintf("found%s", suffix))
		}
		if time.Now().After(deadline) {
			return okResult(fmt.Sprintf("not found within %ds%s", int(d.Seconds()), suffix))
		}
		select {
		case <-ctx.Done():
			return errResult("wait_for cancelled: %v", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// clampWaitTimeout clamps a requested wait to [1,30]s. An unset (0) value
// defaults to the 30s maximum and is NOT reported as clamped; anything outside
// the range is clamped and reported so the result can note it.
func clampWaitTimeout(seconds int) (time.Duration, bool) {
	if seconds == 0 {
		return waitDefaultTimeout, false
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d > waitMaxTimeout:
		return waitMaxTimeout, true
	case d < waitMinTimeout:
		return waitMinTimeout, true
	default:
		return d, false
	}
}

// paginate splits s into pages no larger than limit bytes, never splitting a
// line. A single over-long line becomes its own page. Always returns at least
// one page.
func paginate(s string, limit int) []string {
	if s == "" {
		return []string{""}
	}
	var pages []string
	var cur strings.Builder
	for _, ln := range strings.Split(s, "\n") {
		if cur.Len() > 0 && cur.Len()+len(ln)+1 > limit {
			pages = append(pages, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(ln)
	}
	if cur.Len() > 0 {
		pages = append(pages, cur.String())
	}
	if len(pages) == 0 {
		return []string{""}
	}
	return pages
}

// isProxyDenial reports whether a Chrome navigation errorText is a gate/proxy
// refusal (a CONNECT the gate denied, or a proxy error), as opposed to an
// ordinary load failure.
func isProxyDenial(errText string) bool {
	return strings.Contains(errText, "ERR_TUNNEL_CONNECTION_FAILED") || strings.Contains(errText, "ERR_PROXY")
}

// resolveHref resolves a possibly-relative href against the current page URL.
func resolveHref(base, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return href
	}
	r, err := url.Parse(href)
	if err != nil {
		return href
	}
	return b.ResolveReference(r).String()
}

// hostOf returns the hostname of a URL, or "" if it cannot be parsed.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// attrValue reads an element attribute from a DOM node's flat [name,value,…]
// attribute array; "" if the node is nil or the attribute is absent.
func attrValue(node *cdp.Node, name string) string {
	if node == nil {
		return ""
	}
	for i := 0; i+1 < len(node.Attributes); i += 2 {
		if node.Attributes[i] == name {
			return node.Attributes[i+1]
		}
	}
	return ""
}

// clipFromBox converts a DOM content box quad into a screenshot clip viewport.
func clipFromBox(box *dom.BoxModel) *page.Viewport {
	if box == nil || len(box.Content) < 8 {
		return &page.Viewport{Scale: 1}
	}
	q := box.Content
	minX, minY := q[0], q[1]
	maxX, maxY := q[0], q[1]
	for i := 0; i+1 < len(q); i += 2 {
		minX, maxX = min(minX, q[i]), max(maxX, q[i])
		minY, maxY = min(minY, q[i+1]), max(maxY, q[i+1])
	}
	return &page.Viewport{X: minX, Y: minY, Width: maxX - minX, Height: maxY - minY, Scale: 1}
}

func errResult(format string, a ...any) agenttool.Result {
	return agenttool.Result{Content: fmt.Sprintf(format, a...), IsError: true}
}

func okResult(content string) agenttool.Result {
	return agenttool.Result{Content: content}
}

// runAction runs a single cdproto action against the chromedp-bound context.
func runAction(ctx context.Context, fn func(context.Context) error) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(fn))
}
