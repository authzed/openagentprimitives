package cliout

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPrefixesAndNoColorOnNonTTY(t *testing.T) {
	var b bytes.Buffer
	Step(&b, "ensure %s", "thing")
	OK(&b, "installed %s", "x")
	Warn(&b, "could not %s", "y")
	Errf(&b, "failed %s", "z")
	Info(&b, "detail line")
	out := b.String()

	// A bytes.Buffer is not a terminal → output must be plain (no ANSI escapes),
	// which is what keeps piped output and test assertions stable.
	assert.NotContains(t, out, "\033[", "no ANSI color on a non-terminal writer")

	// Standardized prefixes.
	assert.Contains(t, out, "==> ensure thing\n")
	assert.Contains(t, out, "installed x\n")
	assert.Contains(t, out, "warning: could not y\n")
	assert.Contains(t, out, "error: failed z\n")
	assert.Contains(t, out, "detail line\n")
}
