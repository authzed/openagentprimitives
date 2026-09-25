package steelthread

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// TestEmit_RefusesAnUnresolvedRedaction pins the fail-closed guard between
// Capture and the emitter.
//
// Capture resolves every rule before emitting, so reaching this means a new
// call path skipped that step — and applyRedactions would then DELETE the
// original rather than stand in for it, leaving a bundle with a hole where a
// name should be and a redaction record naming a token that appears nowhere in
// the files it describes.
//
// Internal rather than alongside the rest of the redaction tests because emit
// is the seam being guarded: reaching it through Capture is exactly what is
// impossible, which is why the guard needs a test of its own.
func TestEmit_RefusesAnUnresolvedRedaction(t *testing.T) {
	bundle := bt.Bundle{Name: "demo-lists-widgets", Capture: &bt.Capture{}}

	_, err := emit(bundle, nil, []byte("\n"), nil, []Redaction{{Old: "acme-corp"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no replacement")
	assert.Contains(t, err.Error(), "ResolveRedactions",
		"the refusal has to name the call that was skipped")
}

// TestEmit_AcceptsAResolvedRedaction is the control: the guard above must be
// refusing the unresolved rule specifically, not every rule.
func TestEmit_AcceptsAResolvedRedaction(t *testing.T) {
	bundle := bt.Bundle{Name: "demo-lists-widgets", Capture: &bt.Capture{}}

	res, err := emit(bundle, nil, []byte("\n"), nil, []Redaction{{Old: "acme-corp", New: "COMPANY-A"}})
	require.NoError(t, err)
	require.NotNil(t, res.Bundle.Capture)
	require.Len(t, res.Bundle.Capture.Redactions, 1)
	assert.Equal(t, "COMPANY-A", res.Bundle.Capture.Redactions[0].Replacement)
}
