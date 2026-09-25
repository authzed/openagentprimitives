package llm_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

func TestMIMESet(t *testing.T) {
	s := llm.NewMIMESet(map[string]string{
		"image/png":       llm.NativeBlockImage,
		"application/pdf": llm.NativeBlockDocument,
	})
	assert.True(t, s.Has("image/png"), "declared MIME must be present")
	assert.False(t, s.Has("image/tiff"), "undeclared MIME must be absent")

	// The block type travels WITH the MIME. This is what lets the runner
	// render a native block without ever pattern-matching a MIME string —
	// the property that keeps "add a format" a one-row edit.
	assert.Equal(t, llm.NativeBlockImage, s.BlockType("image/png"))
	assert.Equal(t, llm.NativeBlockDocument, s.BlockType("application/pdf"))
	assert.Empty(t, s.BlockType("image/tiff"), "undeclared MIME has no block type")

	var zero llm.MIMESet
	assert.False(t, zero.Has("image/png"), "zero value must be usable and empty")
	assert.Empty(t, zero.BlockType("image/png"), "zero value must not claim a block type")
}
