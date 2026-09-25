package toolkit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestUnmarshalMinimalToolkit(t *testing.T) {
	data := []byte(`
name: echo
version: "1"
toolkitRevision: "2026-04-24"
target:
  binary: echo
  versionRange: ">=0.0.0"
parser:
  kind: declarative
env:
  allowed: []
subcommands:
  - path: [say]
    description: "say a string"
    positional:
      - {name: message, type: string, required: true}
    flags: []
    effects:
      destructive: false
      reads: []
      writes: []
      network: {destinations: []}
      filesystem: {paths: []}
      creds: {required: [], writes: []}
`)
	var tk Toolkit
	require.NoError(t, yaml.Unmarshal(data, &tk), "unmarshal")
	assert.Equal(t, "echo", tk.Name, "Name")
	assert.Equal(t, "2026-04-24", tk.ToolkitRevision, "ToolkitRevision")
	require.Len(t, tk.Subcommands, 1, "Subcommands length")
	sc := tk.Subcommands[0]
	assert.Equal(t, []string{"say"}, sc.Path, "Subcommand[0].Path")
	require.Len(t, sc.Positional, 1, "Subcommand[0].Positional length")
	assert.Equal(t, "message", sc.Positional[0].Name, "Subcommand[0].Positional[0].Name")
}
