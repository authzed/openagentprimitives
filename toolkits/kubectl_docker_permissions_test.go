package toolkits_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// The kubectl/docker fix gave each DESTRUCTIVE subcommand its own per-subcommand
// permission resolving to stateImpact: external (resourceType k8s_namespace /
// docker_daemon), so `kubectl apply` or `docker run` pauses for an out-of-band
// approval rather than running as unauthorized passthrough.
//
// The loader refuses a `destructive: true` subcommand that resolves to a
// terminal allow, and TestAll_HasCoreSet forces both toolkits through the loader
// — so a PARTIAL revert (keeps destructive:true but weakens the permission)
// panics at load and is caught. But a FULL revert to a toolkit-wide `passthrough`
// with `destructive:true` also removed is self-consistent: it loads clean, and
// the name-presence test stays green. Nothing else asserts the RESOLVED
// stateImpact of these subcommands.
//
// This pins that resolved stateImpact directly, so a full revert (external →
// passthrough) fails here — closing the hole the loader refusal cannot see.
func TestKubectlDockerToolkit_destructiveSubcommandsAreExternal(t *testing.T) {
	cases := []struct {
		toolkit string
		paths   [][]string
	}{
		{
			toolkit: "kubectl",
			// The subcommands that reach the cluster to mutate it.
			paths: [][]string{{"exec"}, {"apply"}, {"delete"}},
		},
		{
			toolkit: "docker",
			// Every docker subcommand that mutates the daemon's state.
			paths: [][]string{
				{"run"}, {"stop"}, {"restart"}, {"kill"}, {"rm"},
				{"push"}, {"rmi"}, {"load"}, {"compose", "up"}, {"compose", "down"},
			},
		},
	}
	for _, tc := range cases {
		tk := findToolkit(t, tc.toolkit)
		for _, path := range tc.paths {
			name := tc.toolkit + " " + strings.Join(path, " ")
			t.Run(name, func(t *testing.T) {
				sc := findSubcommand(t, tk, path)
				p := sc.Permission
				if p == nil {
					p = tk.Permission // toolkit default
				}
				require.NotNil(t, p, "`%s` must declare a permission, not inherit nothing", name)
				assert.Equal(t, authz.External, p.StateImpact,
					"`%s` mutates external state and must resolve to stateImpact=external; a revert "+
						"to passthrough routes it around every approval", name)
				assert.NotNil(t, p.Check,
					"`%s` declares external impact but no check, so nothing scopes the approval to a "+
						"namespace/daemon", name)
			})
		}
	}
}
