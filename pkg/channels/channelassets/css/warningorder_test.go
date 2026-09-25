package css_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
)

// A stylesheet tripping four distinct denylist rules, so the aggregation map
// has enough entries for its iteration order to be visibly random.
const fourRuleSheet = `@import url(https://evil.example/a.css);` +
	`.a{background:url(https://evil.example/p.png);behavior:url(#x);width:expression(1)}`

// TestWarnings_OrderIsDeterministicAcrossRenders is the css kind's half of the
// warning-order fix.
//
// It is a SEPARATE test from the html kind's, and that separation is the point:
// the html renderer sorts the merged list itself, so it stayed deterministic
// even with the CSS sanitizer's own sort removed. This kind returns the
// sanitizer's warnings straight through, so it is the only place that fix is
// observable — and, until this test existed, the only place it could regress
// unnoticed.
func TestWarnings_OrderIsDeterministicAcrossRenders(t *testing.T) {
	render := func() []channelassets.Warning {
		out, err := css.New().Render(context.Background(), channelassets.Input{Payload: []byte(fourRuleSheet)})
		require.NoError(t, err)
		return out.Warnings
	}

	first := render()
	require.GreaterOrEqual(t, len(first), 3, "the fixture must trip several rules, or this asserts nothing")

	for i := range 200 {
		assert.Equal(t, first, render(), "render %d returned the same warnings in a different order", i)
		if t.Failed() {
			break
		}
	}
}

// TestWarnings_OrderIsTheCanonicalOne pins WHICH order, so the css kind and the
// html kind cannot drift into two different deterministic sequences.
func TestWarnings_OrderIsTheCanonicalOne(t *testing.T) {
	out, err := css.New().Render(context.Background(), channelassets.Input{Payload: []byte(fourRuleSheet)})
	require.NoError(t, err)

	want := append([]channelassets.Warning(nil), out.Warnings...)
	channelassets.SortWarnings(want)
	assert.Equal(t, want, out.Warnings, "the css kind emits channelassets' canonical order")
}
