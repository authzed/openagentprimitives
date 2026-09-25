package artifacts_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
	inmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// productionArtifactID is the shape memory.NewID(artifact.Kind{}) emits: the
// kind's prefix plus 8 random bytes, hex-encoded.
var productionArtifactID = regexp.MustCompile(`^artifact-[0-9a-f]{16}$`)

func newIDTestService(t *testing.T, newID func() string) *artifacts.Service {
	t.Helper()
	return artifacts.NewService(memory.NewLocal(inmem.NewBackend()), newID)
}

// A nil newID must leave production behaviour EXACTLY as it was: every binary
// passes nil, and none of them may be able to tell the seam was added.
func TestNewService_NilNewIDKeepsProductionMinting(t *testing.T) {
	svc := newIDTestService(t, nil)

	first, second := svc.NewArtifactID(), svc.NewArtifactID()

	assert.Regexp(t, productionArtifactID, first)
	assert.Regexp(t, productionArtifactID, second)
	assert.NotEqual(t, first, second, "two artifacts must not share a head id")
}

// The seam: the service hands out exactly what the minter returns, in call
// order, so a replay can reproduce the artifact_id a captured run returned to
// the model.
func TestNewService_NewArtifactIDDrawsFromNewID(t *testing.T) {
	var i int
	svc := newIDTestService(t, func() string { i++; return fmt.Sprintf("artifact-%016x", i) })

	assert.Equal(t, "artifact-0000000000000001", svc.NewArtifactID())
	assert.Equal(t, "artifact-0000000000000002", svc.NewArtifactID())
}
