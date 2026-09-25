package toolscmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolsLint_FlatMCP_OK(t *testing.T) {
	path := writeTempValidate(t, `
name: ok
version: "1"
server: { url: https://x, transport: streamable-http }
tools:
  - name: t1
`)
	out, err := runTools(t, "lint", "-f", path)
	require.NoErrorf(t, err, "tools lint; out=%s", out)
	assert.Contains(t, out, "OK", "lint output should report OK")
}

func TestToolsLint_StructuralError(t *testing.T) {
	path := writeTempValidate(t, `
name: bad
version: "1"
# server missing
tools:
  - name: t
`)
	_, err := runTools(t, "lint", "-f", path)
	assert.Error(t, err, "structural error should fail lint")
}
