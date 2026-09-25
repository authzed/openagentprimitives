package agentcmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

func TestAgentPackage_Folder(t *testing.T) {
	dir := t.TempDir()
	src := oaptest.WriteBundle(t)

	t.Run("default extension", func(t *testing.T) {
		out := filepath.Join(dir, "pm.oap")
		cmd := newAgentPackageCmd(&apcmd.Globals{})
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{src, "-o", out})
		require.NoError(t, cmd.Execute())

		packed, err := os.ReadFile(out)
		require.NoError(t, err)

		b, err := oap.Unpack(packed)
		require.NoError(t, err)
		assert.Equal(t, "demo-class", b.Manifest.Agent.Name)

		// Digest + output path are reported on stderr, keeping stdout clean for
		// `-o -` piping.
		assert.Contains(t, stderr.String(), "sha256:")
		assert.Contains(t, stderr.String(), out)
	})

	t.Run("arbitrary extension still unpacks", func(t *testing.T) {
		out := filepath.Join(dir, "pm.custom")
		cmd := newAgentPackageCmd(&apcmd.Globals{})
		cmd.SetArgs([]string{src, "-o", out})
		require.NoError(t, cmd.Execute())

		packed, err := os.ReadFile(out)
		require.NoError(t, err)

		b, err := oap.Unpack(packed)
		require.NoError(t, err)
		assert.Equal(t, "demo-class", b.Manifest.Agent.Name)
	})
}

// TestDefaultPackageOutPath covers the default output name, which is derived
// from the bundle's OWN manifest (agent.name) when no -o is given.
// Manifest.Validate only requires that name to be non-empty — no charset or
// path-element rule — so joining it straight into an os.WriteFile path let a
// folder or cluster bundle write anywhere the invoking user can write, at a
// path of the bundle author's choosing. Same class as the desktop README
// traversal readmeTempPath already closes; that one falls back to a placeholder
// because a menu-bar preview has no way to fail a dialog, while a CLI must
// refuse loudly rather than silently write a differently-named artifact.
func TestDefaultPackageOutPath(t *testing.T) {
	cases := []struct {
		name    string
		agent   string
		want    string
		wantErr bool
	}{
		{
			name:  "plain name: <name>.oap in the current directory",
			agent: "demo-agent",
			want:  "demo-agent.oap",
		},
		{
			name:  "traversing name: reduced to its last path element, never escaping the cwd",
			agent: "../../../../etc/cron.d/x",
			want:  "x.oap",
		},
		{
			name:  "absolute name: reduced to its last path element",
			agent: "/etc/cron.d/x",
			want:  "x.oap",
		},
		{
			name:  "nested name: reduced to its last path element",
			agent: "team/demo-agent",
			want:  "demo-agent.oap",
		},
		{name: "dot: no usable filename, rejected", agent: ".", wantErr: true},
		{name: "dot-dot: no usable filename, rejected", agent: "..", wantErr: true},
		{name: "root: no usable filename, rejected", agent: "/", wantErr: true},
		{name: "empty: no usable filename, rejected", agent: "", wantErr: true},
		{name: "trailing separator only: no usable filename, rejected", agent: "../", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := defaultPackageOutPath(tc.agent)
			if tc.wantErr {
				require.Error(t, err, "a name that yields no usable filename must be refused")
				assert.Contains(t, err.Error(), "-o", "the error must point the user at the flag that fixes it")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, got, filepath.Base(got),
				"the derived path must be a single element — it is written relative to the cwd")
		})
	}
}
