package toolkit_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// A duplicated key silently discards one of the two values — sigs.k8s.io/yaml
// converts to JSON and keeps the LAST. In a toolkit that is an authorization
// spec: two `resourceIDHint`s, or worse two `permission` blocks, and the file
// reads as if it says something it does not.
//
// Not hypothetical. A scripted edit added a second resourceIDHint to `gh repo
// clone`; every test still passed, because the loader quietly took one and the
// file looked fine. It was found by a separate duplicate-key scan, which is not
// something anyone runs by habit.
func TestLoadBytes_rejectsADuplicateKey(t *testing.T) {
	const dup = `
name: demokit
version: "1"
toolkitRevision: "2026-01-01"
target: {binary: demo}
parser: {kind: declarative}
env: {allowed: []}
subcommands:
  - path: [status]
    description: "d"
    permission:
      stateImpact: readonly
      check:
        resourceType: demo_repo
        permission: read
        resourceIDTemplate: "workspace"
        resourceIDHint: "first"
        resourceIDHint: "second"
    effects:
      destructive: false
      reads: [filesystem]
      writes: []
      network: {destinations: []}
      filesystem: {paths: []}
      creds: {required: [], writes: []}
`
	_, err := toolkit.LoadBytes([]byte(dup))
	require.Error(t, err, "a duplicated key must fail the load, not silently keep one value")
	assert.Contains(t, strings.ToLower(err.Error()), "already defined",
		"the error should name the duplicate so the author can find it")
}

// The control: the same file without the duplicate must load.
func TestLoadBytes_acceptsTheSameFileWithoutTheDuplicate(t *testing.T) {
	const ok = `
name: demokit
version: "1"
toolkitRevision: "2026-01-01"
target: {binary: demo}
parser: {kind: declarative}
env: {allowed: []}
subcommands:
  - path: [status]
    description: "d"
    permission:
      stateImpact: readonly
      check:
        resourceType: demo_repo
        permission: read
        resourceIDTemplate: "workspace"
        resourceIDHint: "first"
    effects:
      destructive: false
      reads: [filesystem]
      writes: []
      network: {destinations: []}
      filesystem: {paths: []}
      creds: {required: [], writes: []}
`
	tk, err := toolkit.LoadBytes([]byte(ok))
	require.NoError(t, err)
	assert.Equal(t, "demokit", tk.Name)
}
