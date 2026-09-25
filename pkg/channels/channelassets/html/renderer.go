// Package html is the HTML renderer plug-in. It sanitizes via
// github.com/microcosm-cc/bluemonday under a custom policy, injects a
// restrictive Content-Security-Policy <meta> tag, and emits diff-based
// warnings naming every dropped element and attribute.
package html

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
)

const (
	maxInput  = 256 << 10 // 256 KiB
	maxOutput = 1 << 20   // 1 MiB

	cspContent = `default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline' 'self'; font-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; sandbox allow-same-origin;`
)

// safeDataURIPrefix matches base64 data URIs for raster image formats.
var safeDataURIPrefix = regexp.MustCompile(`^image/(gif|jpeg|png|webp);base64,`)

// svgDataURIPrefix matches a data: URI declaring an SVG image (base64 or
// url-/text-encoded). SVG is allowed ONLY because the consuming context is
// <img src>, where the browser renders it as an image and runs NO script. The
// two SVG vectors that DO execute script stay blocked: an inline <svg> element
// (not in AllowElements → stripped) and a data:svg on <a href> (a top-level
// navigation that executes script — stripped by stripDataNavURLs, since
// bluemonday's data: scheme policy is global and can't tell <img> from <a>).
// We do NOT inspect the SVG body; the <img> rendering context is the boundary.
var svgDataURIPrefix = regexp.MustCompile(`^image/svg\+xml[;,]`)

// TODO(artifacts): support same-origin SVG references too — e.g. an HTML
// artifact <img src>'ing a separately-generated SVG artifact. The CSP already
// allows 'self', but there is no stable artifact-to-artifact URL today, so this
// is data:-only for now. External (cross-origin) image URLs remain blocked by
// the CSP img-src.

// artifactHandlePattern matches a well-formed artifact: reference: one of the
// id prefixes this codebase emits — an ArtifactRender CR name ("ar-…",
// pkg/agent/tool/meta) or a memory artifact/revision id ("artifact-…" /
// "artrev-…", pkg/memory/kinds/artifact,artifactrevision) — optionally with a
// single "#tag" suffix. The charset is deliberately closed ([A-Za-z0-9._-],
// no '/'): the value becomes an authz'd lookup key resolved server-side
// against the session's artifact store, not a path, so '..' / '/' /
// whitespace must never survive.
var artifactHandlePattern = regexp.MustCompile(`^(?:ar|artifact|artrev)-[A-Za-z0-9._-]+$`)

// artifactTagPattern matches the optional "#tag" suffix on an artifact:
// reference. Same closed charset as the handle body.
var artifactTagPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// isArtifactRefValue reports whether s is a well-formed artifact: reference
// body: a handle (see artifactHandlePattern), optionally followed by
// "#" + tag (see artifactTagPattern). Rejects empty, "..", "/", whitespace,
// and any query string or multi-fragment value.
func isArtifactRefValue(s string) bool {
	// The charset ([A-Za-z0-9._-]) allows '.' for legitimate tags like "v1.0",
	// but that means a naive charset+prefix check alone lets dot-runs through
	// ("ar-..", "ar-x#.."). Path traversal must never survive: reject any ".."
	// substring outright, and reject a handle or #tag that is exactly "." or
	// ".." even in isolation (single dots stay allowed everywhere else).
	if strings.Contains(s, "..") {
		return false
	}
	handle, tag, hasTag := strings.Cut(s, "#")
	if handle == "." || handle == ".." {
		return false
	}
	if hasTag {
		if tag == "." || tag == ".." || !artifactTagPattern.MatchString(tag) {
			return false
		}
	}
	return artifactHandlePattern.MatchString(handle)
}

// Renderer is the HTML renderer. Stateless; safe to share across goroutines.
type Renderer struct {
	policy *bluemonday.Policy
}

// New constructs a Renderer with the sanitization policy.
func New() *Renderer {
	p := bluemonday.UGCPolicy()
	p.RequireParseableURLs(true)
	p.AllowURLSchemes("http", "https", "mailto")
	// A tightened data: URI policy, narrower than bluemonday's own
	// AllowDataURIImages: base64 raster images and SVG-as-image only. The
	// navigational data:svg vector is removed afterward by stripDataNavURLs,
	// since a scheme policy is global and cannot tell <img src> from <a href>.
	p.AllowURLSchemeWithCustomPolicy("data", func(u *url.URL) bool {
		if u.RawQuery != "" || u.Fragment != "" {
			return false
		}
		// Raster images: require base64 + valid encoding.
		if matched := safeDataURIPrefix.FindString(u.Opaque); matched != "" {
			_, err := base64.StdEncoding.DecodeString(u.Opaque[len(matched):])
			return err == nil
		}
		// SVG-as-image: safe in <img src> (no script execution). Body is not
		// inspected; the image rendering context is the safety boundary.
		return svgDataURIPrefix.MatchString(u.Opaque)
	})
	// artifact:HANDLE — an internal reference to a same-session secondary
	// artifact, resolved server-side (live-view URL / bundle-relative / inline)
	// before the browser ever fetches it. Validated for handle shape here; the
	// scheme allowlist is global across linkable attrs, so stripDataNavURLs
	// (below) removes it from navigational attributes afterward, keeping it only
	// on img[src] / link[href].
	p.AllowURLSchemeWithCustomPolicy("artifact", func(u *url.URL) bool {
		if u.RawQuery != "" {
			return false
		}
		// net/url.Parse already splits a "#tag" suffix out of u.Opaque into
		// u.Fragment; reassemble so isArtifactRefValue sees the shape it
		// documents (handle, or handle#tag). One harmless divergence from
		// validating the raw attribute string: a trailing bare "#" ("ar-x#")
		// parses to Fragment=="" and reassembles to "ar-x", so it passes where
		// the raw string would fail on the empty tag. Meaningless input only,
		// not a validation gap.
		v := u.Opaque
		if u.Fragment != "" {
			v += "#" + u.Fragment
		}
		return isArtifactRefValue(v)
	})
	// <link rel="stylesheet" href="artifact:…"> — bluemonday's UGCPolicy drops
	// <link> entirely; allow it narrowly (rel=stylesheet + href only).
	p.AllowAttrs("rel").Matching(regexp.MustCompile(`^stylesheet$`)).OnElements("link")
	p.AllowAttrs("href").OnElements("link")
	p.AllowElements("link")
	// Allow relative (fragment) URLs such as <a href="#section">. These are safe
	// in our context: artifacts are served in a sandboxed iframe with CSP
	// default-src 'none', so a relative href can only navigate within the same
	// opaque document — no resource load, no cross-origin navigation, no
	// exfiltration. Fragment links are needed by the css kind's :target tab UI.
	p.AllowRelativeURLs(true)
	p.RequireNoFollowOnLinks(true)
	p.AllowElements("style")
	p.AllowAttrs("style").Globally()
	// UGCPolicy strips `class` because it assumes untrusted users must not
	// style their own content. For agent-authored reports that is
	// counter-productive: the agent's <style> rules target classes on the
	// elements it just emitted, so stripping the hooks kills the layout
	// (panels, pills, tables) while leaving the rules intact. Defense-in-depth
	// lives in the injected CSP (default-src 'none' + sandbox
	// allow-same-origin) and the already-allowed style attribute, not here.
	p.AllowAttrs("class").Globally()
	// Safe element set for agent-authored UI/docs. UGCPolicy omits several
	// inert HTML5 elements (notably <main>/<header>/<footer>/<nav>); stripping
	// them UNWRAPS the tag and drops its class/id hook, silently breaking
	// class-based layout. The denied complement (script, iframe, object,
	// embed, svg, audio/video, base/link/meta/title, …) stays stripped, and
	// unknown/nonstandard elements remain stripped — safe by default.
	p.AllowElements(
		// sectioning / document
		"main", "header", "footer", "nav", "hgroup", "address",
		// grouping / flow
		"figure", "figcaption",
		// inline text semantics UGCPolicy may omit
		"data", "time", "mark", "bdi", "bdo", "wbr", "ruby", "rt", "rp",
		"var", "samp", "kbd",
		// images
		"picture", "source",
		// interactive-inert
		"details", "summary", "dialog",
		// form controls (inert here: on*/formaction stripped, CSP form-action 'none')
		"form", "label", "fieldset", "legend", "button", "input", "select",
		"option", "optgroup", "textarea", "datalist", "output", "progress", "meter",
	)
	// UGCPolicy constrains title to its `Paragraph` regex, which rejects common
	// tooltip punctuation (`:`, `$`, `%`, `?`, …) and so SILENTLY STRIPS the
	// whole attribute on any non-trivial tooltip. Re-add it unconstrained: a
	// later global policy with a nil matcher makes the attr always-allowed,
	// since bluemonday keeps an attr if ANY of its policies match. Safe for the
	// same reason `style`/`class` are global above — title is inert tooltip
	// text, HTML-escaped on output (no attribute breakout), behind the injected
	// CSP + sandboxed iframe. id/lang/dir KEEP UGCPolicy's value-format
	// constraints; those formats are semantically required.
	p.AllowAttrs("title").Globally()
	// Other safe global attributes UGCPolicy omits.
	p.AllowAttrs("role").Globally()
	p.AllowAttrs("aria-hidden", "aria-label", "aria-labelledby", "aria-describedby",
		"aria-live", "aria-current", "aria-expanded", "aria-controls",
		"aria-roledescription").Globally()
	// Per-element safe attributes.
	p.AllowAttrs("datetime").OnElements("time", "ins", "del")
	p.AllowAttrs("value").OnElements("data", "input", "option", "progress", "meter", "li")
	// NOT srcset: bluemonday only URL-validates href/cite/src, so srcset would
	// pass any value (remote URLs / non-image schemes) at the allowlist layer —
	// the remote-reference surface we deliberately deny. The CSP backstops it
	// (img-src 'self' data:), and a <source> without srcset is inert, so the
	// element stays allowed but its responsive-image URLs do not.
	p.AllowAttrs("media", "type").OnElements("source")
	p.AllowAttrs("loading", "decoding").OnElements("img")
	p.AllowAttrs("start", "reversed").OnElements("ol")
	p.AllowAttrs("open").OnElements("details", "dialog")
	p.AllowAttrs("colspan", "rowspan", "headers", "scope").OnElements("td", "th")
	p.AllowAttrs("span").OnElements("col", "colgroup")
	p.AllowAttrs("for").OnElements("label", "output")
	p.AllowAttrs("name").OnElements("form", "input", "select", "textarea", "button", "output", "meter")
	p.AllowAttrs("placeholder", "checked", "disabled", "readonly", "min", "max", "step", "multiple", "selected").
		OnElements("input", "button", "select", "option", "textarea")
	p.AllowAttrs("type").OnElements("input", "button") // type has no meaning on select/option/textarea
	p.AllowAttrs("min", "max", "low", "high", "optimum").OnElements("meter", "progress")
	// Stripping the dangerous attrs (action, formaction) can leave a form
	// element attribute-free, and bluemonday drops attr-free elements unless
	// AllowNoAttrs names them. The global class/id/aria-* attrs cover most
	// cases; this handles the residual bare <form>.
	p.AllowNoAttrs().OnElements("form", "label", "legend", "output", "main", "dialog", "data", "source",
		"fieldset", "optgroup", "datalist")
	// AllowUnsafe is required for <style> content to pass through: bluemonday
	// hard-codes <script>/<style> as content-skipped otherwise. <script> stays
	// stripped regardless, being absent from AllowElements. Defense-in-depth:
	// the injected CSP (default-src 'none' + sandbox) blocks resource loads
	// from CSS, and the iframe sandbox isolates execution.
	p.AllowUnsafe(true)
	return &Renderer{policy: p}
}

func (Renderer) Kind() string { return "html" }
func (Renderer) ExecutionMode() channelassets.ExecutionMode {
	return channelassets.ExecutionModeInProcess
}
func (Renderer) Delivery() channelassets.DeliveryMode { return channelassets.DeliveryStandalone }
func (Renderer) MaxInputSize() int64                  { return maxInput }
func (Renderer) MaxOutputSize() int64                 { return maxOutput }
func (Renderer) OutputMIMEs() []string                { return []string{"text/html"} }
func (Renderer) InputMIMEs() []string                 { return []string{"text/html"} }
func (Renderer) SupportsLiveView() bool               { return true }
func (Renderer) AgentSelectable() bool                { return true }

// Instructions is the HTML kind's authoring guidance for the agent. Keep it in
// sync with New()'s sanitization policy and injectCSP.
func (Renderer) Instructions() string {
	return "HTML is sanitized and a Content-Security-Policy is applied before delivery. " +
		"Allowed: full document/sectioning layout (`main`, `header`, `footer`, `nav`, `section`, `article`, `aside`, `figure`, etc.), " +
		"headings, tables, lists, inline text semantics, `details`/`summary`/`dialog`, form controls " +
		"(`form`, `input`, `button`, … — they render but are inert: submission is blocked and JS won't run), " +
		"images (raster `data:` URIs, an SVG as `<img src=\"data:image/svg+xml,...\">`, or same-origin), " +
		"inline `<style>` and `style=\"...\"` with any modern CSS (custom properties, `var()`, `clamp()`, grid, `@media`, `@keyframes`), " +
		"and http/https/mailto links. " +
		"Stripped: scripts, event handlers (`on*`), `iframe`/`object`/`embed`, inline `<svg>` elements " +
		"(embed SVG via `<img src=\"data:image/svg+xml,...\">` instead — an inline `<svg>` can carry JavaScript), " +
		"`data:` links, and from CSS the risky bits only (`@import`, remote `url(...)`, `expression()`, `behavior`). " +
		"JavaScript will not run; build with HTML + CSS. " +
		"For images and shared CSS, PREFER a referenced asset over inlining: create the asset as its own artifact " +
		"(`image`/`svg`/`css` kind) and reference it by handle — `<img src=\"artifact:HANDLE\">` or " +
		"`<link rel=\"stylesheet\" href=\"artifact:HANDLE\">`, where HANDLE is the artifact_prepare handle/id. " +
		"Small inline SVG vectors are fine (they are tiny). NEVER hand-emit a base64-encoded raster image inline — " +
		"it is token-catastrophic and can trigger a content-policy refusal mid-response. Get raster bytes without typing them: " +
		"generate via code execution then `artifact_prepare(source: container_file)`, or, when the asset comes from another " +
		"tool call (a file fetched from a git repo / download), pass that tool output's ArtifactRef to " +
		"`artifact_prepare(source: tool_output)`. Then reference the returned handle. " +
		"The `warnings` field reports EXACTLY what was changed and how: `unwrapped` = a tag was dropped but its content kept, " +
		"so any `class`/`id` on that tag is gone and class-based styling on it will not apply — move that styling onto an allowed " +
		"element or an inline `style=`; `removed` = the element and its content were deleted; `stripped` = an attribute or CSS " +
		"construct was removed. Read `warnings` after each render and adjust the source before re-rendering."
}

func (r *Renderer) Render(ctx context.Context, in channelassets.Input) (channelassets.Output, error) {
	if int64(len(in.Payload)) > maxInput {
		return channelassets.Output{}, fmt.Errorf("input %d bytes > max %d", len(in.Payload), maxInput)
	}
	if err := ctx.Err(); err != nil {
		return channelassets.Output{}, err
	}

	if endsInsideTag(in.Payload) {
		return channelassets.Output{}, fmt.Errorf(
			"%w: payload ends inside an unterminated tag or attribute — the response was likely cut off; regenerate the complete document",
			channelassets.ErrMalformedPayload)
	}
	if isFullyEscaped(in.Payload) {
		return channelassets.Output{}, fmt.Errorf(
			"%w: payload is HTML-escaped text, not markup — emit raw <tag> elements, not &lt;tag&gt;",
			channelassets.ErrMalformedPayload)
	}

	preTags, preAttrs := tokenize(in.Payload)
	sanitized := r.policy.SanitizeBytes(in.Payload)

	withCSP, cssWarnings, err := injectCSP(sanitized)
	if err != nil {
		return channelassets.Output{}, fmt.Errorf("inject CSP: %w", err)
	}

	if int64(len(withCSP)) > maxOutput {
		return channelassets.Output{}, fmt.Errorf("output %d bytes > max %d", len(withCSP), maxOutput)
	}

	// Compute removals against the FINAL delivered bytes so the reconstructed
	// <head>/<html>/<body> are never reported as changes. injectCSP also adds
	// exactly one <meta http-equiv="Content-Security-Policy" content="…">; we
	// discount it from the post-counts so a user's own stripped <meta>/
	// http-equiv/content (all denied) is reported accurately instead of being
	// masked by our injected one.
	postTags, postAttrs := tokenize(withCSP)
	discount(postTags, "meta")
	discount(postAttrs, "http-equiv")
	discount(postAttrs, "content")
	warnings := append(diffWarnings(preTags, postTags, preAttrs, postAttrs), cssWarnings...)
	// diffWarnings walks two COUNT MAPS, and Go randomizes map iteration, so
	// without this two renders of identical bytes returned the same warnings in
	// a different order. The list is model-facing (artifact_prepare's result)
	// and lands on ArtifactRender.status, so that was a real difference between
	// two identical runs. Sorted here, once, over the diff and CSS warnings
	// together — see channelassets.SortWarnings.
	channelassets.SortWarnings(warnings)

	filename := in.Filename
	if filename == "" {
		filename = "asset.html"
	} else if !strings.HasSuffix(strings.ToLower(filename), ".html") &&
		!strings.HasSuffix(strings.ToLower(filename), ".htm") {
		filename += ".html"
	}

	return channelassets.Output{
		Bytes:    withCSP,
		MIME:     "text/html",
		Filename: filename,
		AltText:  in.AltText,
		Warnings: warnings,
	}, nil
}

// endsInsideTag reports whether the payload ends while still inside an
// unterminated tag or a quoted attribute value — the signature of a truncated
// document (e.g. an inline data: URI cut off mid-stream). A lone '<' only opens
// tag-scan for a plausible tag start (name / '/' / '!' / '?'), so a bare '<' in
// text does not trip it. This is a deliberate single-pass heuristic, not a full
// parser, and has known blind spots: a bare un-encoded '<' at the very end of
// the payload (indistinguishable here from a truncated tag), and '>' appearing
// inside an HTML comment (which this scanner does not special-case).
func endsInsideTag(b []byte) bool {
	inTag := false
	var quote byte // 0, '"', or '\''
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inTag {
			if quote != 0 {
				if c == quote {
					quote = 0
				}
				continue
			}
			switch c {
			case '"', '\'':
				quote = c
			case '>':
				inTag = false
			}
			continue
		}
		if c == '<' {
			if i+1 >= len(b) {
				inTag = true // trailing '<' at EOF
				break
			}
			n := b[i+1]
			if n == '/' || n == '!' || n == '?' ||
				(n >= 'a' && n <= 'z') || (n >= 'A' && n <= 'Z') {
				inTag = true
			}
		}
	}
	return inTag
}

// isFullyEscaped reports whether the payload is HTML-entity-encoded markup
// rather than raw markup — parsing yields no element nodes beyond the
// structural html/head/body wrappers html.Parse always inserts, AND the raw
// payload contains the escaped tag-opener "&lt;". Such a payload would render
// as literal source, not a page. The "&lt;" half of the test is required:
// ordinary tag-less plain text ("All checks passed.") also parses to zero
// element nodes yet must render normally.
func isFullyEscaped(b []byte) bool {
	if !bytes.Contains(b, []byte("&lt;")) {
		return false
	}
	doc, err := xhtml.Parse(bytes.NewReader(b))
	if err != nil {
		return false // a parse error is not this failure mode; let it render/sanitize
	}
	elems := 0
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			switch n.DataAtom {
			case atom.Html, atom.Head, atom.Body:
				// structural wrappers html.Parse always inserts
			default:
				elems++
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return elems == 0
}

func tokenize(b []byte) (tagCount, attrCount map[string]int) {
	tagCount = map[string]int{}
	attrCount = map[string]int{}
	z := xhtml.NewTokenizer(bytes.NewReader(b))
	for {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			return
		}
		switch tt {
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tagCount[string(name)]++
			if hasAttr {
				for {
					k, _, more := z.TagAttr()
					attrCount[string(k)]++
					if !more {
						break
					}
				}
			}
		}
	}
}

// discount decrements one occurrence of name from a tokenize count map (used
// to subtract injectCSP's own injected <meta> contributions before diffing, so
// they don't mask a user's stripped same-named element/attribute).
func discount(counts map[string]int, name string) {
	if counts[name] > 0 {
		counts[name]--
	}
}

// skipContentElements MUST exactly mirror bluemonday's
// setOfElementsToSkipContent (policy.go addDefaultSkipElementContent, v1.0.27):
// these are dropped WITH their content ("removed"); every other disallowed
// element is "unwrapped" (tag dropped, children kept). Note: <applet> is NOT
// here — bluemonday unwraps it, keeping its content.
var skipContentElements = map[string]struct{}{
	"frame": {}, "frameset": {}, "iframe": {}, "noembed": {}, "noframes": {},
	"noscript": {}, "nostyle": {}, "object": {}, "script": {}, "style": {},
	"title": {},
}

func diffWarnings(preTags, postTags, preAttrs, postAttrs map[string]int) []channelassets.Warning {
	var out []channelassets.Warning
	for name, n := range preTags {
		dropped := n - postTags[name]
		if dropped <= 0 {
			continue
		}
		action := "unwrapped"
		note := "content kept; class/id hook on this tag is gone — move its styling to an allowed element or an inline style="
		if _, skip := skipContentElements[name]; skip {
			action = "removed"
			note = ""
		}
		out = append(out, channelassets.Warning{
			Kind: "tag", Name: name, Action: action, Count: dropped, Note: note,
		})
	}
	for name, n := range preAttrs {
		dropped := n - postAttrs[name]
		if dropped <= 0 {
			continue
		}
		out = append(out, channelassets.Warning{
			Kind: "attr", Name: name, Action: "stripped", Count: dropped,
		})
	}
	return out
}

func injectCSP(htmlBytes []byte) ([]byte, []channelassets.Warning, error) {
	doc, err := xhtml.Parse(bytes.NewReader(htmlBytes))
	if err != nil {
		return nil, nil, err
	}
	// Strip data: from navigational attributes, and artifact: from every
	// attribute except img[src]/link[href]. bluemonday's URL-scheme allowlist
	// is global across all linkable attributes (a/area/base/link.href,
	// blockquote/q/del/ins.cite, img/source.src, …), so it cannot itself
	// distinguish "rendered as an image" from "navigated to" — that
	// distinction is enforced here, post-sanitize.
	stripDataNavURLs(doc)
	cssWarnings := sanitizeCSSNodes(doc)
	head := findOrCreateHead(doc)
	if head == nil {
		return nil, nil, fmt.Errorf("internal: no head node available for CSP injection")
	}
	meta := &xhtml.Node{
		Type:     xhtml.ElementNode,
		DataAtom: atom.Meta,
		Data:     "meta",
		Attr: []xhtml.Attribute{
			{Key: "http-equiv", Val: "Content-Security-Policy"},
			{Key: "content", Val: cspContent},
		},
	}
	if head.FirstChild == nil {
		head.AppendChild(meta)
	} else {
		head.InsertBefore(meta, head.FirstChild)
	}
	var buf bytes.Buffer
	if err := xhtml.Render(&buf, doc); err != nil {
		return nil, nil, err
	}
	// xhtml.Render escapes single quotes to &#39; even inside a double-quoted
	// attribute value, where escaping is unnecessary. CSP source-list keywords
	// ('none', 'self', 'unsafe-inline') are literal single quotes; browsers
	// parse the escaped form fine, so restoring them is purely for legibility.
	escaped := strings.ReplaceAll(cspContent, "'", "&#39;")
	rendered := bytes.ReplaceAll(buf.Bytes(),
		[]byte(`content="`+escaped+`"`),
		[]byte(`content="`+cspContent+`"`))
	return rendered, cssWarnings, nil
}

func findOrCreateHead(n *xhtml.Node) *xhtml.Node {
	if n.Type == xhtml.ElementNode && n.DataAtom == atom.Head {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if got := findOrCreateHead(c); got != nil {
			return got
		}
	}
	html := findHTML(n)
	if html == nil {
		return nil
	}
	head := &xhtml.Node{Type: xhtml.ElementNode, DataAtom: atom.Head, Data: "head"}
	html.InsertBefore(head, html.FirstChild)
	return head
}

// stripDataNavURLs walks the parsed tree and removes:
//   - `href`/`cite` attributes whose value is a data: URL. Navigating to a
//     data: document (via a link) executes script for SVG/HTML payloads —
//     unlike <img src>, where SVG renders as an image. img `src` data: URLs
//     are left intact.
//   - `artifact:` reference values from every attribute except `img[src]`
//     and `link[href]` — the only two contexts resolved server-side before
//     delivery. Everywhere else (nav `href`/`area href`, `cite`, a defensive
//     `src` on any non-`img` element) the reference is dropped rather than
//     left to reach the browser unresolved.
//   - every `link[href]` value that is NOT an `artifact:` reference —
//     including otherwise-allowed schemes (http/https) that the global URL
//     policy would let through. There is no legitimate use for an external
//     stylesheet on `<link>` (inline CSS uses `<style>`), so `link[href]` is
//     scoped to `artifact:` only, not just filtered for `data:`/`artifact:`.
func stripDataNavURLs(n *xhtml.Node) {
	if n.Type == xhtml.ElementNode && len(n.Attr) > 0 {
		kept := n.Attr[:0]
		for _, a := range n.Attr {
			switch strings.ToLower(a.Key) {
			case "href", "cite":
				if isDataURL(a.Val) {
					continue // drop the navigational data: URL
				}
				if n.DataAtom == atom.Link {
					// link[href] has no use case for a non-artifact: value: external
					// stylesheets are blocked by design (inline CSS uses <style>), so
					// only an artifact: reference survives here.
					if !isArtifactURLValue(a.Val) {
						continue
					}
				} else if isArtifactURLValue(a.Val) {
					continue // artifact: survives ONLY on link[href]
				}
			case "src":
				if isArtifactURLValue(a.Val) && n.DataAtom != atom.Img {
					continue // artifact: survives ONLY on img[src]
				}
			}
			kept = append(kept, a)
		}
		n.Attr = kept
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		stripDataNavURLs(c)
	}
}

// isDataURL reports whether v is a data: URL, tolerant of leading whitespace
// and case (browsers trim and lower-case the scheme).
func isDataURL(v string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimLeft(v, " \t\r\n\f")), "data:")
}

// isArtifactURLValue reports whether v is an artifact: URL, tolerant of
// leading whitespace and case (mirrors isDataURL).
func isArtifactURLValue(v string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimLeft(v, " \t\r\n\f")), "artifact:")
}

func findHTML(n *xhtml.Node) *xhtml.Node {
	if n.Type == xhtml.ElementNode && n.DataAtom == atom.Html {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if got := findHTML(c); got != nil {
			return got
		}
	}
	return nil
}
