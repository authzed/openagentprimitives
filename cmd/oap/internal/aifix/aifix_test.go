package aifix

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
)

// withLookPath swaps the package PATH probe for the duration of a test, stubbing
// which CLIs are "installed" without requiring a real binary on PATH.
func withLookPath(t *testing.T, present map[string]string) {
	t.Helper()
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(name string) (string, error) {
		if p, ok := present[name]; ok {
			return p, nil
		}
		return "", errors.New("not found")
	}
}

func TestRegistry_RegistersTheThreeBuiltins(t *testing.T) {
	got := Registered()
	names := make([]string, 0, len(got))
	for _, l := range got {
		names = append(names, l.Name())
	}
	// All three built-in launchers self-register via init().
	assert.Contains(t, names, "claude")
	assert.Contains(t, names, "codex")
	assert.Contains(t, names, "gemini")
}

func TestFirstAvailable(t *testing.T) {
	cases := []struct {
		name     string
		present  map[string]string
		wantName string
		wantOK   bool
	}{
		{
			name:    "none installed: not available",
			present: map[string]string{},
			wantOK:  false,
		},
		{
			name:     "only gemini installed: returns gemini",
			present:  map[string]string{"gemini": "/usr/bin/gemini"},
			wantName: "gemini",
			wantOK:   true,
		},
		{
			name:     "multiple installed: returns first in registration order",
			present:  map[string]string{"codex": "/usr/bin/codex", "gemini": "/usr/bin/gemini"},
			wantName: "codex", // codex registers before gemini
			wantOK:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withLookPath(t, tc.present)
			l, ok := FirstAvailable()
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				require.NotNil(t, l)
				assert.Equal(t, tc.wantName, l.Name())
			}
		})
	}
}

func TestDetect_ReflectsLookPath(t *testing.T) {
	withLookPath(t, map[string]string{"claude": "/opt/bin/claude"})
	l := cliLauncher{name: "claude", argsFor: positionalArg}
	path, ok := l.Detect()
	require.True(t, ok)
	assert.Equal(t, "/opt/bin/claude", path)

	missing := cliLauncher{name: "nope", argsFor: positionalArg}
	_, ok = missing.Detect()
	assert.False(t, ok)
}

func TestBuildPrompt_IncludesRepoRootComponentAndDiagnostics(t *testing.T) {
	diag := wait.Diagnosis{
		Pod:      "postgres-0",
		PodPhase: "Pending",
		Reason:   "ContainerCreating",
		PVCs:     []wait.PVCNote{{Name: "data-postgres-0", Phase: "Pending"}},
		Events: []wait.EventNote{
			{Reason: "FailedAttachVolume", Message: "could not attach pd-balanced disk", Count: 3},
		},
	}
	prompt := BuildPrompt("/home/dev/agentprimitives", "postgres", diag, "kind-ap", "gke")

	assert.Contains(t, prompt, "/home/dev/agentprimitives", "prompt must point at the repo root")
	assert.Contains(t, prompt, "postgres", "prompt must name the failing component")
	assert.Contains(t, prompt, "kind-ap", "prompt must include the kube-context")
	assert.Contains(t, prompt, "gke", "prompt must include the detected cloud")
	assert.Contains(t, prompt, "FailedAttachVolume", "prompt must include a diagnostic event reason")
	assert.Contains(t, prompt, "could not attach pd-balanced disk", "prompt must include the diagnostic event message")
	assert.Contains(t, prompt, "data-postgres-0", "prompt must include the PVC state")
	assert.Contains(t, strings.ToLower(prompt), "fix", "prompt must instruct the agent to fix")

	lower := strings.ToLower(prompt)
	assert.Contains(t, lower, "do not edit", "prompt must forbid editing the install code")
	assert.Contains(t, lower, "findings", "prompt must instruct writing findings to a file")
	assert.Contains(t, lower, "follow-up agent", "prompt must frame the file as a handoff to a follow-up agent")
}

func TestBuildPrompt_OmitsEmptyContextAndDegradesGracefully(t *testing.T) {
	// No kube-context, no cloud, empty diagnosis: prompt is still well-formed.
	prompt := BuildPrompt("/repo", "graphiti", wait.Diagnosis{}, "", "")
	assert.Contains(t, prompt, "/repo")
	assert.Contains(t, prompt, "graphiti")
	assert.NotContains(t, prompt, "Kube-context:")
	assert.NotContains(t, prompt, "Cloud:")
	assert.Contains(t, prompt, "no specific diagnostics")
}

func TestNames_ListsRegisteredLaunchers(t *testing.T) {
	n := Names()
	assert.Contains(t, n, "claude")
	assert.Contains(t, n, "codex")
	assert.Contains(t, n, "gemini")
}
