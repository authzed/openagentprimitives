package runner_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/modality"
	"github.com/authzed/openagentprimitives/pkg/agent/modality/files"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

// TestModalityInstructions_GatesOnToolPresenceNotEnv proves ModalityInstructions
// decides from mergedTools, not merely from env being "capable" — the exact
// property internal/cmd/runner's fix round needed: env.Reader can be wired (Tier-1
// available in principle) while fetch_artifact is absent from mergedTools
// (no capability actually offered it this turn), and that must yield no
// instructions, not a false-positive "here's how to use fetch_artifact" block
// for a tool the agent doesn't have.
func TestModalityInstructions_GatesOnToolPresenceNotEnv(t *testing.T) {
	env := modality.Env{Reader: files.StoreReader{Store: blobstore.NewMem()}}

	t.Run("fetch_artifact present in mergedTools -> files modality instructions included", func(t *testing.T) {
		mergedTools := []tool.Tool{files.NewFetchArtifact(env.Reader)}
		got := runner.ModalityInstructions(env, mergedTools)
		require.Len(t, got, 1)
		assert.Contains(t, got[0], "fetch_artifact")
	})

	t.Run("fetch_artifact absent from mergedTools despite a wired Reader -> no instructions", func(t *testing.T) {
		got := runner.ModalityInstructions(env, nil)
		assert.Empty(t, got)
	})

	t.Run("a differently-named tool in mergedTools does not count as present", func(t *testing.T) {
		mergedTools := []tool.Tool{&fakeTool{name: "respond_to_user", kind: tool.KindMeta}}
		got := runner.ModalityInstructions(env, mergedTools)
		assert.Empty(t, got)
	})
}

// TestModalityInstructions_NilReaderStillOmitsWhenToolAbsent guards the
// symmetric case: no Reader wired at all (Tier-1 truly unavailable) AND the
// tool absent — the common/default case for most sessions.
func TestModalityInstructions_NilReaderStillOmitsWhenToolAbsent(t *testing.T) {
	got := runner.ModalityInstructions(modality.Env{}, nil)
	assert.Empty(t, got)
}
