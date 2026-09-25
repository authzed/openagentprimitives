package installcmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckCommandExists(t *testing.T) {
	root := newRoot(t)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"check", "--help"})
	require.NoError(t, root.Execute(), "check --help")
	assert.Contains(t, stdout.String(), "every registered component's health", "check --help should describe the command")
	assert.Contains(t, stdout.String(), "--repair", "check --help should advertise the repair flag")
}
