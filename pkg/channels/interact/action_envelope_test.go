package interact

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildActionEnvelope_DelimitsUntrustedFields(t *testing.T) {
	out, err := buildActionEnvelope(mcpUiActionPayload{Type: "link", URL: "https://example.com/report"})
	require.NoError(t, err)

	assert.Contains(t, out, `<untrusted-widget-action nonce="`)
	open := between(t, out, `<untrusted-widget-action nonce="`, `"`)
	require.NotEmpty(t, open)
	assert.Contains(t, out, `</untrusted-widget-action nonce="`+open+`">`)
	assert.Contains(t, out, "https://example.com/report")
}

func TestBuildActionEnvelope_NonceIsFreshPerAction(t *testing.T) {
	a, err := buildActionEnvelope(mcpUiActionPayload{Type: "notify", Message: "x"})
	require.NoError(t, err)
	b, err := buildActionEnvelope(mcpUiActionPayload{Type: "notify", Message: "x"})
	require.NoError(t, err)
	assert.NotEqual(t,
		between(t, a, `nonce="`, `"`), between(t, b, `nonce="`, `"`),
		"each action must get an unpredictable fresh nonce")
}
