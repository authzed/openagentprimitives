package all_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

func TestAll_RegistersArtifactKinds(t *testing.T) {
	for _, name := range []string{"artifact", "artifact_revision"} {
		_, ok := memory.LookupKind(name)
		assert.Truef(t, ok, "Kind %q must be registered when pkg/memory/kinds/all is imported", name)
	}
}
