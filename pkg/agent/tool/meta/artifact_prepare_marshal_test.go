package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Internal-package test: marshalArtifactPrepareResult and artifactPrepareResult
// are unexported, so this lives in package meta (not meta_test).
func TestMarshalArtifactPrepareResult_StructuredWarnings(t *testing.T) {
	tr, err := marshalArtifactPrepareResult(artifactPrepareResult{
		Handle: "h", Status: "ready",
		Warnings: []spiceboxv1alpha1.SanitizerWarning{
			{Kind: "tag", Name: "main", Action: "unwrapped", Count: 1, Note: "hook lost"},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, tr.Content, `"action":"unwrapped"`)
	assert.Contains(t, tr.Content, `"name":"main"`)
	assert.True(t, tr.Trusted, "artifact_prepare is a framework meta tool and must opt out of content-guard inspection")
}
