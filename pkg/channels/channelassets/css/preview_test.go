package css_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	csskind "github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
)

// Compile-time: Renderer satisfies PreviewComposer.
var _ channelassets.PreviewComposer = csskind.Renderer{}

const sampleCSS = `.panel { background: #fff; padding: 1rem; border-radius: 4px; }
.heading { font-size: 1.5rem; color: #333; }
.item { margin: .25rem 0; }`

// fakeGen is a MarkupGenerator that always returns the given markup.
func fakeGen(markup string) channelassets.MarkupGenerator {
	return func(_ context.Context, _ string) (string, error) {
		return markup, nil
	}
}

// errorGen is a MarkupGenerator that always returns an error.
func errorGen() channelassets.MarkupGenerator {
	return func(_ context.Context, _ string) (string, error) {
		return "", errors.New("llm unavailable")
	}
}

// TestPreviewHTML_WithFakeGen pins the preview document's structure when a
// generator supplies the sample markup: the generated markup (checked by a
// substring that survives bluemonday), the artifact CSS in a <style> element,
// both tab links, and the chroma-highlighted source block.
func TestPreviewHTML_WithFakeGen(t *testing.T) {
	r := csskind.New()
	gen := fakeGen(`<div class="panel">hi</div>`)
	out, err := r.PreviewHTML(context.Background(), []byte(sampleCSS), gen)
	require.NoError(t, err, "PreviewHTML with fake gen")

	body := string(out)
	// Generated markup (after sanitization the div + class survives).
	assert.Contains(t, body, "panel", "generated markup class must appear in output")
	// Artifact CSS in a <style> block.
	assert.Contains(t, body, sampleCSS, "artifact CSS must appear in a <style> block")
	// Tab navigation links.
	assert.Contains(t, body, `href="#preview"`, "preview tab link must be present")
	assert.Contains(t, body, `href="#source"`, "source tab link must be present")
	// Section ids.
	assert.Contains(t, body, `id="preview"`, "preview section id must be present")
	assert.Contains(t, body, `id="source"`, "source section id must be present")
	// Chroma source: the highlighted block will contain the CSS text.
	assert.Contains(t, body, "panel", "chroma source block must include CSS selector text")
}

// TestPreviewHTML_WithNilGen verifies that the fallback scaffold is used when
// gen is nil: the output must be non-empty and contain the tab structure.
func TestPreviewHTML_WithNilGen(t *testing.T) {
	r := csskind.New()
	out, err := r.PreviewHTML(context.Background(), []byte(sampleCSS), nil)
	require.NoError(t, err, "PreviewHTML with nil gen")

	body := string(out)
	assert.NotEmpty(t, body, "output must not be empty")
	assert.Contains(t, body, `href="#preview"`, "preview tab link must be present even with nil gen")
	assert.Contains(t, body, `href="#source"`, "source tab link must be present even with nil gen")
	assert.Contains(t, body, `id="preview"`, "preview section id must be present")
	assert.Contains(t, body, `id="source"`, "source section id must be present")
	assert.Contains(t, body, "<style>", "artifact CSS must appear in a <style> block")
}

// TestPreviewHTML_WithErrorGen verifies that a gen that returns an error falls
// back to the deterministic scaffold (same structural expectations as nil gen).
func TestPreviewHTML_WithErrorGen(t *testing.T) {
	r := csskind.New()
	out, err := r.PreviewHTML(context.Background(), []byte(sampleCSS), errorGen())
	require.NoError(t, err, "PreviewHTML with error gen must not itself error")

	body := string(out)
	assert.Contains(t, body, `href="#preview"`, "preview tab link must be present with error gen")
	assert.Contains(t, body, `href="#source"`, "source tab link must be present with error gen")
	assert.Contains(t, body, "<style>", "style block must be present")
}

// TestPreviewHTML_SurvivesHTMLSanitization is the critical integration test:
// the composed preview goes through the html kind's sanitizer, which must
// leave the tab links, the <style> blocks and the section ids intact. A
// failure here means the preview renders as a dead page — the tabs stop
// navigating (AllowRelativeURLs) or lose their styling.
func TestPreviewHTML_SurvivesHTMLSanitization(t *testing.T) {
	r := csskind.New()
	out, err := r.PreviewHTML(context.Background(), []byte(sampleCSS), nil)
	require.NoError(t, err, "PreviewHTML")

	htmlR := html.New()
	sanitized, err := htmlR.Render(context.Background(), channelassets.Input{Payload: out})
	require.NoError(t, err, "html Render of preview doc")

	body := string(sanitized.Bytes)

	// Tab navigation fragment links — the key test for AllowRelativeURLs.
	assert.Contains(t, body, `href="#preview"`, "CRITICAL: #preview fragment href must survive html sanitization")
	assert.Contains(t, body, `href="#source"`, "CRITICAL: #source fragment href must survive html sanitization")

	// <style> blocks must survive (AllowUnsafe + AllowElements("style")).
	assert.Contains(t, body, "<style>", "CRITICAL: <style> blocks must survive html sanitization")

	// Section ids must survive (id is allowed by UGCPolicy).
	assert.Contains(t, body, `id="preview"`, "CRITICAL: section id=preview must survive html sanitization")
	assert.Contains(t, body, `id="source"`, "CRITICAL: section id=source must survive html sanitization")

	// The artifact CSS itself must still appear inside the style block.
	assert.Contains(t, body, "panel", "artifact CSS class selector must survive in the style block")
}

// TestClassesIn_ExtractsClassNames covers class extraction indirectly:
// classesIn is unexported, so it is observed through the class names its
// output puts into PreviewHTML's nil-gen fallback scaffold.
func TestClassesIn_ExtractsClassNames(t *testing.T) {
	css := `.foo { color: red } .bar-baz { margin: 0 } .foo { padding: 0 } #not-class { display: none }`
	r := csskind.New()
	out, err := r.PreviewHTML(context.Background(), []byte(css), nil)
	require.NoError(t, err)

	body := string(out)
	// The fallback scaffold uses extracted classes as class= values.
	assert.Contains(t, body, "foo", "first extracted class must appear in fallback scaffold")
	assert.Contains(t, body, "bar-baz", "second extracted class must appear in fallback scaffold")
}

// TestPreviewHTML_TypeAssertable confirms runtime type-assertion to PreviewComposer works.
func TestPreviewHTML_TypeAssertable(t *testing.T) {
	var r channelassets.Renderer = csskind.New()
	pc, ok := r.(channelassets.PreviewComposer)
	assert.True(t, ok, "css Renderer must satisfy channelassets.PreviewComposer")
	assert.NotNil(t, pc)

	out, err := pc.PreviewHTML(context.Background(), []byte(sampleCSS), nil)
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(out), `href="#preview"`))
}

// TestPreviewHTML_ModelMarkupIsSanitized proves the defense-in-depth pass over
// model output: a <script> tag returned by gen never reaches the final
// document.
func TestPreviewHTML_ModelMarkupIsSanitized(t *testing.T) {
	r := csskind.New()
	maliciousGen := fakeGen(`<div class="panel">safe</div><script>alert(1)</script>`)
	out, err := r.PreviewHTML(context.Background(), []byte(sampleCSS), maliciousGen)
	require.NoError(t, err)

	body := string(out)
	assert.NotContains(t, body, "<script", "script injected by gen must be sanitized out")
	assert.Contains(t, body, "safe", "non-script content from gen must survive sanitization")
}
