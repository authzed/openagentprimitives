package artifactrevision_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/artifactrevision"
)

func TestArtifactRevisionKind_RegistryAndShape(t *testing.T) {
	k, ok := memory.LookupKind("artifact_revision")
	require.True(t, ok, "artifact_revision Kind must be registered via init()")
	assert.Equal(t, "artrev-", k.IDPrefix())
	assert.Equal(t, reflect.TypeOf(artifactrevision.Revision{}), k.ContentSchema())

	r := artifactrevision.Revision{Seq: 2, ChangeDescription: "added chart", RenderName: "ar-x", OutputRef: "mem://o", MIME: "text/html", Size: 12}
	raw, err := json.Marshal(r)
	require.NoError(t, err, "marshal Revision")
	var back artifactrevision.Revision
	require.NoError(t, json.Unmarshal(raw, &back), "unmarshal Revision")
	assert.Equal(t, r, back, "Revision must round-trip through JSON")
}
