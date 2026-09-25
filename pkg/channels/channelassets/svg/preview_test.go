package svg_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	svgkind "github.com/authzed/openagentprimitives/pkg/channels/channelassets/svg"
)

// Compile-time: Renderer satisfies PreviewComposer.
var _ channelassets.PreviewComposer = svgkind.Renderer{}

const sampleSVG = `<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10" fill="red"/></svg>`

func TestPreviewHTML_ContainsDataURI(t *testing.T) {
	r := svgkind.New()
	out, err := r.PreviewHTML(context.Background(), []byte(sampleSVG), nil)
	require.NoError(t, err, "PreviewHTML")
	body := string(out)

	expected := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(sampleSVG))
	assert.Contains(t, body, expected, "output must contain the base64 data URI of the SVG content")
	assert.Contains(t, body, "<img", "output must contain an <img> element")
}

// TestPreviewHTML_SurvivesHTMLSanitization feeds the composed preview document
// through the html kind's sanitizer and asserts the <img> with its data URI
// survives. This is the critical integration check: if the html kind strips the
// data:image/svg+xml URI, the preview is broken.
func TestPreviewHTML_SurvivesHTMLSanitization(t *testing.T) {
	r := svgkind.New()
	previewDoc, err := r.PreviewHTML(context.Background(), []byte(sampleSVG), nil)
	require.NoError(t, err, "PreviewHTML")

	htmlR := html.New()
	out, err := htmlR.Render(context.Background(), channelassets.Input{Payload: previewDoc})
	require.NoError(t, err, "html Render")

	body := string(out.Bytes)
	assert.Contains(t, body, "data:image/svg+xml;base64,", "<img> with data URI must survive html sanitization")
	assert.Contains(t, body, "<img", "<img> element must survive html sanitization")
}

// TestPreviewHTML_GenIsUnused confirms that a non-nil gen is ignored (the SVG
// preview is always deterministic regardless of what gen returns).
func TestPreviewHTML_GenIsUnused(t *testing.T) {
	called := false
	gen := func(_ context.Context, _ string) (string, error) {
		called = true
		return "<p>generated</p>", nil
	}
	r := svgkind.New()
	_, err := r.PreviewHTML(context.Background(), []byte(sampleSVG), gen)
	require.NoError(t, err)
	assert.False(t, called, "gen must not be called for the svg preview")
}

// TestPreviewHTML_EmptyContentGraceful confirms that empty content doesn't panic.
func TestPreviewHTML_EmptyContentGraceful(t *testing.T) {
	r := svgkind.New()
	out, err := r.PreviewHTML(context.Background(), []byte{}, nil)
	require.NoError(t, err)
	assert.Contains(t, string(out), "data:image/svg+xml;base64,")
}

// TestPreviewHTML_TypeAssertable confirms that a value returned from New() can
// be type-asserted to channelassets.PreviewComposer at runtime, as consumers
// like the live-viewer will do.
func TestPreviewHTML_TypeAssertable(t *testing.T) {
	var r channelassets.Renderer = svgkind.New()
	pc, ok := r.(channelassets.PreviewComposer)
	assert.True(t, ok, "svg Renderer must satisfy channelassets.PreviewComposer")
	assert.NotNil(t, pc)

	out, err := pc.PreviewHTML(context.Background(), []byte(sampleSVG), nil)
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(out), "data:image/svg+xml;base64,"))
}
