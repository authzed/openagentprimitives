package css

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"

	chromaquick "github.com/alecthomas/chroma/v2/quick"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	htmlkind "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
)

// Compile-time assertion: Renderer satisfies the PreviewComposer interface.
var _ channelassets.PreviewComposer = Renderer{}

// classNameRE matches CSS class selectors and captures the class name.
var classNameRE = regexp.MustCompile(`\.([A-Za-z_][\w-]*)`)

// classesIn returns the class names in a CSS string, de-duplicated and in
// first-seen order. The result only seeds the LLM instruction or the fallback
// scaffold, so completeness is best-effort.
func classesIn(css string) []string {
	matches := classNameRE.FindAllStringSubmatch(css, -1)
	seen := make(map[string]struct{}, len(matches))
	var out []string
	for _, m := range matches {
		name := m[1]
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// fallbackScaffold returns fixed HTML body markup carrying a sampling of the
// extracted class names. Used when gen is nil or errors.
func fallbackScaffold(classes []string) string {
	// Up to 4 class names, sprinkled through the markup so the preview actually
	// exercises the stylesheet; with none, the structural HTML still stands.
	pick := func(i int, def string) string {
		if i < len(classes) {
			return classes[i]
		}
		return def
	}
	c0 := pick(0, "container")
	c1 := pick(1, "heading")
	c2 := pick(2, "item")
	c3 := pick(3, "note")
	return fmt.Sprintf(
		`<div class="%s"><h1 class="%s">Preview</h1>`+
			`<p class="%s">This is a paragraph exercising the stylesheet.</p>`+
			`<ul><li class="%s">Item one</li><li class="%s">Item two</li><li class="%s">Item three</li></ul>`+
			`<table><tr><th class="%s">Name</th><th class="%s">Value</th></tr>`+
			`<tr><td class="%s">alpha</td><td class="%s">1</td></tr>`+
			`<tr><td class="%s">beta</td><td class="%s">2</td></tr></table>`+
			`</div>`,
		c0, c1,
		c2,
		c3, c3, c3,
		c1, c1,
		c2, c2,
		c2, c2,
	)
}

// PreviewHTML composes the HTML preview document for a CSS artifact: two tabs
// built from :target CSS selectors, no JS — #preview applies the artifact's
// CSS to sample markup, #source shows it chroma-highlighted.
//
// A non-nil gen produces the sample markup from the extracted class names; a
// nil gen or a gen error falls back to a deterministic scaffold. Either way
// the markup goes through the html kind's sanitizer before embedding, because
// model output is never trusted.
func (Renderer) PreviewHTML(ctx context.Context, content []byte, gen channelassets.MarkupGenerator) ([]byte, error) {
	cssStr := string(content)

	// 1. Extract class names to seed the sample markup instruction / fallback.
	classes := classesIn(cssStr)

	// 2. Sample markup: try the generator, fall back deterministically.
	var sampleMarkup string
	if gen != nil {
		classList := strings.Join(classes, ", ")
		instruction := "Write HTML body markup only (no <html>/<head>/<body>/<script> tags). " +
			"Use only safe HTML elements (no script, no iframe). " +
			"The markup should exercise these CSS classes: " + classList + ". " +
			"Include a heading, paragraph, unordered list, table, and a couple of divs."
		generated, err := gen(ctx, instruction)
		if err == nil && strings.TrimSpace(generated) != "" {
			sampleMarkup = generated
		}
	}
	if sampleMarkup == "" {
		sampleMarkup = fallbackScaffold(classes)
	}

	// 3. Sanitize the sample markup through the html kind (defense-in-depth).
	htmlR := htmlkind.New()
	sanitizedOut, err := htmlR.Render(ctx, channelassets.Input{Payload: []byte(sampleMarkup)})
	if err != nil {
		// Sample markup is decoration, so a sanitize failure degrades to a
		// visible placeholder rather than failing the whole preview.
		sanitizedOut.Bytes = []byte("<p>Preview unavailable.</p>")
	}
	// The sanitizer wraps its output in <html><head><body>, so take the body
	// contents to avoid nesting whole documents. Falling back to the full
	// output when there are no body tags is still safe.
	sanitizedMarkup := extractBody(sanitizedOut.Bytes)

	// 4. Highlight source with chroma (CSS lexer, html formatter, inline styles).
	var chromaBuf bytes.Buffer
	if err := chromaquick.Highlight(&chromaBuf, cssStr, "css", "html", "monokai"); err != nil {
		// Fallback: plain-text code block with HTML escaping.
		chromaBuf.Reset()
		chromaBuf.WriteString("<pre><code>")
		chromaBuf.WriteString(htmlEscape(cssStr))
		chromaBuf.WriteString("</code></pre>")
	}
	highlightedSource := chromaBuf.String()

	// 5. Compose the :target-tab document. Tabs work without JavaScript:
	// clicking <a href="#preview"> sets the URL fragment, the browser applies
	// :target to <section id="preview">, and the sibling rules show/hide
	// sections. These fragment hrefs survive html sanitization only because of
	// AllowRelativeURLs(true) in the html renderer. With no target, preview is
	// visible and source hidden.
	tabCSS := `
<style>
.ap-tabs-nav{display:flex;gap:.5rem;padding:.5rem;background:#1e1e1e;border-bottom:1px solid #444}
.ap-tabs-nav a{color:#ccc;text-decoration:none;padding:.25rem .75rem;border-radius:4px;font-family:sans-serif;font-size:.875rem}
.ap-tabs-nav a:hover{background:#333;color:#fff}
.ap-tab{display:none;padding:1rem}
/* default: show preview when no target is active */
.ap-tab#preview{display:block}
/* :target rules override the default */
.ap-tab:target{display:block}
/* hide preview when source is targeted, and vice versa */
#source:target~#preview,
#preview:target~#source{display:none}
/* also hide preview when source is targeted (handle ordering) */
body:has(#source:target) #preview{display:none}
body:has(#preview:target) #source{display:none}
</style>`

	var buf bytes.Buffer
	buf.WriteString("<!doctype html><html><head><title>CSS preview</title>")
	buf.WriteString(tabCSS)
	buf.WriteString("</head><body>")
	buf.WriteString(`<nav class="ap-tabs-nav">`)
	buf.WriteString(`<a href="#preview">Preview</a>`)
	buf.WriteString(`<a href="#source">Source</a>`)
	buf.WriteString("</nav>")

	// Preview section: the artifact CSS + sanitized sample markup.
	buf.WriteString(`<section class="ap-tab" id="preview">`)
	buf.WriteString("<style>")
	buf.WriteString(cssStr)
	buf.WriteString("</style>")
	buf.Write(sanitizedMarkup)
	buf.WriteString("</section>")

	// Source section: chroma-highlighted CSS.
	buf.WriteString(`<section class="ap-tab" id="source">`)
	buf.WriteString(highlightedSource)
	buf.WriteString("</section>")

	buf.WriteString("</body></html>")

	return buf.Bytes(), nil
}

// extractBody returns the inner HTML of the <body> element from a full HTML
// document, or the full input if no <body> is found.
func extractBody(b []byte) []byte {
	s := string(b)
	startTag := "<body>"
	endTag := "</body>"
	start := strings.Index(s, startTag)
	end := strings.LastIndex(s, endTag)
	if start == -1 || end == -1 || end <= start {
		return b
	}
	return []byte(s[start+len(startTag) : end])
}

// htmlEscape escapes the five special HTML characters so the CSS can be
// embedded inside a <code> block safely.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&#34;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}
