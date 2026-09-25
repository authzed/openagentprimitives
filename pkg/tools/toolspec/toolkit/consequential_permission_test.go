package toolkit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A subcommand that declares CONSEQUENCE and resolves to a terminal-allow
// permission is a self-contradicting toolkit, and the contradiction is
// mechanically detectable.
//
// passthrough and stateless are terminal allows at every gate: the tool checker
// returns Allowed with no SpiceDB call, permsurface gives them severity 0 and
// mints no handle, so nothing enters the permission surface, nothing is
// declarable in a plan, and nothing renders on a card. The plan gate says so
// itself — "returns ok=false for calls with no handle (meta tools, passthrough
// tools) — those are not a plan-gate concern."
//
// kubectl shipped exactly this shape: a single toolkit-wide `stateImpact:
// passthrough`, inherited by every subcommand including `apply` and `delete`,
// which the SAME FILE marks destructive: true. The YAML contradicted itself and
// nothing read it. `kubectl apply -f -` with a cluster-admin ClusterRoleBinding
// on stdin was allowed with no check, no card and no audit handle, while the
// identical mutation through gh or git is external and re-approved per call.
//
// This is the check that catches the next one at authoring time.
func TestValidate_AConsequentialSubcommandCannotResolveToATerminalAllow(t *testing.T) {
	const head = `
name: demo
version: "1"
toolkitRevision: "2026-09-09"
target: {binary: demo}
parser: {kind: declarative}
`
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "destructive subcommand inheriting a passthrough toolkit default",
			yaml: head + `
permission: {stateImpact: passthrough}
subcommands:
  - path: [nuke]
    effects: {destructive: true}
`,
			wantErr: "destructive",
		},
		{
			name: "destructive subcommand overriding to stateless explicitly",
			yaml: head + `
subcommands:
  - path: [nuke]
    permission: {stateImpact: stateless}
    effects: {destructive: true}
`,
			wantErr: "destructive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.yaml))
			require.Error(t, err, "a toolkit that contradicts itself must not load")
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// The other half: the shapes that are correct must keep loading. A read-only
// subcommand under a passthrough default is the ordinary case and is not a
// contradiction, and a consequential one that names a real permission is
// exactly what the rule asks for.
func TestValidate_ConsequentialSubcommandsWithARealPermissionStillLoad(t *testing.T) {
	const head = `
name: demo
version: "1"
toolkitRevision: "2026-09-09"
target: {binary: demo}
parser: {kind: declarative}
`
	cases := []struct{ name, yaml string }{
		{
			name: "a reading subcommand under a passthrough default",
			yaml: head + `
permission: {stateImpact: passthrough}
subcommands:
  - path: [get]
    effects: {reads: [network]}
`,
		},
		{
			name: "a destructive subcommand that routes to a human",
			yaml: head + `
permission: {stateImpact: passthrough}
subcommands:
  - path: [nuke]
    permission: {stateImpact: external}
    effects: {destructive: true}
`,
		},
		{
			name: "a writing subcommand that is authorization-checked",
			yaml: head + `
permission: {stateImpact: passthrough}
subcommands:
  - path: [push]
    permission: {stateImpact: readwrite}
    effects: {destructive: true, writes: [network]}
`,
		},
		{
			// effects.writes alone is NOT the trigger, deliberately: "network"
			// in these toolkits routinely means "talks to its own daemon or API
			// server", and a rule that refused every one of those would fire on
			// the safe majority and get switched off.
			name: "a non-destructive subcommand that writes, under a passthrough default",
			yaml: head + `
permission: {stateImpact: passthrough}
subcommands:
  - path: [pull]
    effects: {writes: [network]}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.yaml))
			assert.NoError(t, err)
		})
	}
}
