package html

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestServeTransform_ServesArtifactInert(t *testing.T) {
	in := []byte(`<html><head><meta http-equiv="Content-Security-Policy" content="` + cspContent + `"></head><body>hello</body></html>`)
	out := string(Renderer{}.ServeTransform(in))

	// The artifact is the INNER, script-disabled frame of the host page, which
	// owns all bridge behavior: nothing is injected here and the baked CSP is
	// served unchanged.
	assert.Equal(t, string(in), out, "ServeTransform must serve the artifact bytes unchanged (inert)")
	assert.NotContains(t, out, "<script", "no bridge script may be injected into the artifact")
	assert.NotContains(t, out, "script-src", "no script-src may be added; default-src 'none' governs")
}
