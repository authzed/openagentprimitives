package html_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
)

func renderWarnings(t *testing.T, payload string) []channelassets.Warning {
	t.Helper()
	out, err := html.New().Render(context.Background(), channelassets.Input{Payload: []byte(payload)})
	require.NoError(t, err, "Render")
	return out.Warnings
}

func findWarning(ws []channelassets.Warning, kind, name string) (channelassets.Warning, bool) {
	for _, w := range ws {
		if w.Kind == kind && w.Name == name {
			return w, true
		}
	}
	return channelassets.Warning{}, false
}

func TestWarnings_HeadAndStyleNotReported(t *testing.T) {
	// A reconstructed <head> and a surviving <style> must NOT be reported as
	// stripped — a warning naming them sends the agent chasing a phantom.
	ws := renderWarnings(t, `<html><head><style>.x{color:blue}</style></head><body><div class="x">hi</div></body></html>`)
	_, headReported := findWarning(ws, "tag", "head")
	_, styleReported := findWarning(ws, "tag", "style")
	assert.False(t, headReported, "head must not be reported as stripped; got %v", ws)
	assert.False(t, styleReported, "style must not be reported as stripped; got %v", ws)
}

func TestWarnings_DeniedElementClassification(t *testing.T) {
	// <iframe> and <script> are content-skipping → "removed".
	ws := renderWarnings(t, `<iframe src="https://evil.example"></iframe><script>x</script>`)
	w, ok := findWarning(ws, "tag", "iframe")
	require.True(t, ok, "iframe warning present; got %v", ws)
	assert.Equal(t, "removed", w.Action)
}

func TestWarnings_UnwrappedClassification(t *testing.T) {
	// An unknown/disallowed non-content-skipping element is "unwrapped"
	// (content kept). <center> is obsolete and not in the allowlist.
	ws := renderWarnings(t, `<center class="c">hi</center>`)
	w, ok := findWarning(ws, "tag", "center")
	require.True(t, ok, "center warning present; got %v", ws)
	assert.Equal(t, "unwrapped", w.Action)
	assert.NotEmpty(t, w.Note, "unwrapped warning should carry an actionable note")
}

func TestWarnings_AppletUnwrappedNotRemoved(t *testing.T) {
	// <applet> is NOT in bluemonday's skip-content set, so bluemonday unwraps
	// it and keeps the content — classify it "unwrapped", never "removed".
	out, err := html.New().Render(context.Background(), channelassets.Input{
		Payload: []byte(`<applet code="evil.class">kept text</applet>`),
	})
	require.NoError(t, err, "Render")
	assert.Contains(t, string(out.Bytes), "kept text", "applet content must survive (unwrapped)")
	w, ok := findWarning(out.Warnings, "tag", "applet")
	require.True(t, ok, "applet warning present; got %v", out.Warnings)
	assert.Equal(t, "unwrapped", w.Action)
}

func TestWarnings_CSSDenylistReported(t *testing.T) {
	ws := renderWarnings(t, `<style>@import url(https://evil.example/x.css);.a{color:red}</style><p>x</p>`)
	w, ok := findWarning(ws, "css", "@import")
	require.True(t, ok, "css @import warning present; got %v", ws)
	assert.Equal(t, "stripped", w.Action)
}

func TestWarnings_InlineStyleSanitized(t *testing.T) {
	// Inline style= remote url() is stripped + reported; the element survives.
	ws := renderWarnings(t, `<div style="background:url(https://evil.example/p.png)">x</div>`)
	_, ok := findWarning(ws, "css", "url()")
	assert.True(t, ok, "inline-style url() warning present; got %v", ws)
}

func TestWarnings_MetaCountNotMaskedByInjectedCSP(t *testing.T) {
	// injectCSP adds its own <meta>; a user's two stripped <meta> tags must be
	// reported as 2, not masked down to 1 by the injected CSP meta.
	ws := renderWarnings(t, `<html><head><meta charset="utf-8"><meta name="viewport" content="x"></head><body>hi</body></html>`)
	w, ok := findWarning(ws, "tag", "meta")
	require.True(t, ok, "meta warning present; got %v", ws)
	assert.Equal(t, 2, w.Count, "both user metas reported despite injected CSP meta")
}
