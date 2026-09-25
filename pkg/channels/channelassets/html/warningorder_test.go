package html_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
)

// The document that found it: a full page whose <html lang>, <meta charset> and
// <title> are all stripped or rewritten, producing four warnings from two map
// diffs. Four is the smallest count at which the randomization is reliably
// visible — two tags and two attributes, so both loops have something to
// permute.
const fourWarningPage = `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
	`<title>PR review</title></head><body><h1>Review</h1><p>Two findings.</p></body></html>`

// TestWarnings_OrderIsDeterministicAcrossRenders is the regression test for a
// bug that was invisible for as long as nothing compared two runs.
//
// diffWarnings walks two maps of counts, and Go randomizes map iteration, so
// identical bytes rendered twice returned the same warnings in a different
// order. Nothing in the renderer's own tests noticed, because they all look
// warnings up by name. It surfaced only when a captured session was replayed
// and the entire artifact_prepare result matched except for the sequence of
// four warnings.
//
// The list is model-facing — artifact_prepare returns it, and it is mirrored
// onto ArtifactRender.status.warnings — so this was a real difference between
// two identical runs, not an internal detail.
//
// Many iterations rather than two: one comparison passes by luck at 1/4 for
// this document, and a loop that would pass by luck is not a regression test.
func TestWarnings_OrderIsDeterministicAcrossRenders(t *testing.T) {
	first := renderWarnings(t, fourWarningPage)
	require.Len(t, first, 4, "the fixture must actually produce several warnings, or this asserts nothing")

	for i := range 200 {
		assert.Equal(t, first, renderWarnings(t, fourWarningPage),
			"render %d returned the same warnings in a different order", i)
		if t.Failed() {
			break
		}
	}
}

// TestWarnings_OrderIsTheCanonicalOne pins WHICH order, not just that there is
// one. A renderer that sorted by some private rule would be deterministic and
// still disagree with the encoder that re-expresses a recorded result
// (meta.CanonicalizeResult), which is the join this order exists to hold.
func TestWarnings_OrderIsTheCanonicalOne(t *testing.T) {
	got := renderWarnings(t, fourWarningPage)
	want := append([]channelassets.Warning(nil), got...)
	channelassets.SortWarnings(want)
	assert.Equal(t, want, got, "the renderer emits channelassets' canonical order")
}

// TestWarnings_CSSOrderIsDeterministic covers the other producer: the CSS
// sanitizer aggregates its own findings in a map too, and its warnings are
// appended to the HTML renderer's, so both had to be fixed for either to be
// stable.
func TestWarnings_CSSOrderIsDeterministic(t *testing.T) {
	const page = `<style>@import url(https://evil.example/a.css);` +
		`.a{background:url(https://evil.example/p.png);behavior:url(#x);width:expression(1)}</style><p>x</p>`

	out, err := html.New().Render(context.Background(), channelassets.Input{Payload: []byte(page)})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(out.Warnings), 3, "the fixture must trip several CSS rules")

	for range 200 {
		again, err := html.New().Render(context.Background(), channelassets.Input{Payload: []byte(page)})
		require.NoError(t, err)
		require.Equal(t, out.Warnings, again.Warnings, "CSS warnings must not permute between renders")
	}
}
