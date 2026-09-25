package svg_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	svgkind "github.com/authzed/openagentprimitives/pkg/channels/channelassets/svg"
)

// emptySVG is the placeholder returned when validation fails. Must stay in
// lockstep with the constant in render.go.
const emptySVG = `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"></svg>`

// TestRender_Unsafe_BlanksAndWarns covers the adversarial corpus: every input
// here is an attempt to smuggle script, navigation, HTML, or external refs into
// an SVG. None may hard-fail (err must be nil); each must render the empty SVG
// placeholder AND carry at least one warning naming the offending construct.
func TestRender_Unsafe_BlanksAndWarns(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"inline <script>", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`},
		{"on* event handler", `<svg xmlns="http://www.w3.org/2000/svg"><rect onload="alert(1)" width="1" height="1"/></svg>`},
		{"foreignObject HTML smuggling", `<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><body xmlns="http://www.w3.org/1999/xhtml">x</body></foreignObject></svg>`},
		{"<a> javascript nav", `<svg xmlns="http://www.w3.org/2000/svg"><a href="javascript:alert(1)"><text>x</text></a></svg>`},
		{"animate rewrites href", `<svg xmlns="http://www.w3.org/2000/svg"><animate attributeName="href" to="javascript:alert(1)"/></svg>`},
		{"<use> external ref", `<svg xmlns="http://www.w3.org/2000/svg"><use href="http://evil.example/x.svg"/></svg>`},
		{"<image> external ref", `<svg xmlns="http://www.w3.org/2000/svg"><image href="http://evil.example/x.png"/></svg>`},
		{"non-svg garbage", `not svg at all`},
		{"XXE external entity", `<?xml version="1.0"?><!DOCTYPE svg [<!ENTITY x SYSTEM "file:///etc/passwd">]><svg xmlns="http://www.w3.org/2000/svg"><text>&x;</text></svg>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svgkind.New().Render(context.Background(), channelassets.Input{Payload: []byte(tc.payload)})
			require.NoError(t, err, "unsafe SVG must never hard-fail Render")
			assert.Equal(t, emptySVG, string(out.Bytes), "unsafe SVG must render the empty placeholder")
			assert.Equal(t, "image/svg+xml", out.MIME)
			assert.NotEmpty(t, out.Warnings, "unsafe SVG must carry at least one warning")
		})
	}
}

// TestRender_Safe_PassesThroughUnchanged covers the safe corpus: every input is
// a legitimate, presentation-only SVG. The renderer validates but never
// rewrites — so the output bytes must be byte-identical to the input, with no
// warnings.
func TestRender_Safe_PassesThroughUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"rect with fill", `<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10" fill="red"/></svg>`},
		{"path with stroke", `<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0 L10 10" stroke="black"/></svg>`},
		{"circle", `<svg xmlns="http://www.w3.org/2000/svg"><circle cx="5" cy="5" r="3" fill="blue"/></svg>`},
		{"text", `<svg xmlns="http://www.w3.org/2000/svg"><text x="0" y="10">hello</text></svg>`},
		{"camelCase linearGradient + stop", `<svg xmlns="http://www.w3.org/2000/svg"><linearGradient id="g"><stop offset="0%" stop-color="red"/><stop offset="100%" stop-color="blue"/></linearGradient></svg>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svgkind.New().Render(context.Background(), channelassets.Input{
				Payload: []byte(tc.payload),
				AltText: "alt",
			})
			require.NoError(t, err)
			assert.Equal(t, tc.payload, string(out.Bytes), "safe SVG must pass through byte-identical")
			assert.Equal(t, "image/svg+xml", out.MIME)
			assert.Empty(t, out.Warnings, "safe SVG must produce no warnings")
			assert.Equal(t, "alt", out.AltText)
		})
	}
}

// TestRender_OverInputSize_HardErrors is the ONE hard error path: input larger
// than MaxInputSize is rejected outright (not blanked).
func TestRender_OverInputSize_HardErrors(t *testing.T) {
	r := svgkind.New()
	big := make([]byte, r.MaxInputSize()+1)
	_, err := r.Render(context.Background(), channelassets.Input{Payload: big})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max")
}

// TestRender_FilenameGetsSVGExt confirms the .svg extension helper.
func TestRender_FilenameGetsSVGExt(t *testing.T) {
	r := svgkind.New()
	safe := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"></svg>`)

	out, err := r.Render(context.Background(), channelassets.Input{Payload: safe, Filename: "diagram"})
	require.NoError(t, err)
	assert.Equal(t, "diagram.svg", out.Filename)

	out, err = r.Render(context.Background(), channelassets.Input{Payload: safe, Filename: "diagram.svg"})
	require.NoError(t, err)
	assert.Equal(t, "diagram.svg", out.Filename)

	out, err = r.Render(context.Background(), channelassets.Input{Payload: safe})
	require.NoError(t, err)
	assert.Equal(t, "image.svg", out.Filename)
}

// TestRendererContract pins the static contract surface.
func TestRendererContract(t *testing.T) {
	r := svgkind.New()
	assert.Equal(t, "svg", r.Kind())
	assert.Equal(t, channelassets.DeliveryBundledOnly, r.Delivery())
	assert.True(t, r.SupportsLiveView())
	assert.Equal(t, channelassets.ExecutionModeInProcess, r.ExecutionMode())
	assert.Equal(t, []string{"image/svg+xml"}, r.OutputMIMEs())
	assert.Equal(t, []string{"image/svg+xml"}, r.InputMIMEs())
	assert.NotEmpty(t, r.Instructions())
	// ServeTransform is a no-op pass-through.
	content := []byte("payload")
	assert.Equal(t, content, r.ServeTransform(content))
}
