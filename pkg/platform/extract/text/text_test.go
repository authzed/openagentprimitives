package text_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
	_ "github.com/authzed/openagentprimitives/pkg/platform/extract/text"
)

func TestExtract_ClaimedMIMEs_RoundTripText(t *testing.T) {
	cases := []struct {
		name string
		mime string
	}{
		{name: "plain text", mime: "text/plain"},
		{name: "markdown", mime: "text/markdown"},
		{name: "csv", mime: "text/csv"},
		{name: "json", mime: "application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := extract.For(tc.mime)
			require.True(t, ok, "MIME %q must be registered", tc.mime)

			got, err := e.Extract(strings.NewReader("hello, world"))
			require.NoError(t, err)
			assert.Equal(t, "hello, world", got.Text)
			assert.Equal(t, 0, got.Pages, "plain text has no page concept")
		})
	}
}

func TestExtract_InvalidUTF8_CoercedNotErrored(t *testing.T) {
	e, ok := extract.For("text/plain")
	require.True(t, ok)

	invalid := []byte("valid text \xff\xfe more valid text")
	got, err := e.Extract(bytes.NewReader(invalid))
	require.NoError(t, err, "invalid bytes are coerced, not rejected")
	assert.Contains(t, got.Text, "valid text")
	assert.Contains(t, got.Text, "more valid text")
	assert.NotContains(t, got.Text, "\xff", "invalid byte sequences must be replaced, not passed through raw")
}

func TestExtract_OversizedInput_ReturnsErrTooLarge(t *testing.T) {
	e, ok := extract.For("text/plain")
	require.True(t, ok)

	oversized := bytes.Repeat([]byte("a"), 25*1024*1024+1) // one byte past the documented 25 MiB cap
	_, err := e.Extract(bytes.NewReader(oversized))
	assert.ErrorIs(t, err, extract.ErrTooLarge)
}
