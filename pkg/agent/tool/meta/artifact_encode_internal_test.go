package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// In-package, because the property is about the encoder's ARGUMENT and the only
// way to observe it is to hold the slice yourself. Routing through the tool
// cannot see it: the fake client deep-copies on Create and Get, so the status
// slice the tool sorts is already a copy of anything a test handed in, and an
// assertion out there passes whether the encoder clones or not.
func TestEncodeArtifactPrepareResult_DoesNotMutateItsArgument(t *testing.T) {
	ws := []spiceboxv1alpha1.SanitizerWarning{
		{Kind: "tag", Name: "title", Action: "removed", Count: 1},
		{Kind: "attr", Name: "lang", Action: "stripped", Count: 1},
		{Kind: "tag", Name: "meta", Action: "unwrapped", Count: 1},
	}
	before := append([]spiceboxv1alpha1.SanitizerWarning(nil), ws...)

	body, err := encodeArtifactPrepareResult(artifactPrepareResult{
		Handle: "ar-sess1-aaaaaa", Status: "ready", Warnings: ws,
	})
	require.NoError(t, err)

	assert.Equal(t, before, ws,
		"an encoder must not reorder the slice it was handed: the caller passes its "+
			"fetched CR's status straight in and goes on using it")

	// And the output IS sorted, so the clone is not just a no-op that happens
	// to leave the input alone.
	assert.Less(t,
		indexOf(string(body), `"name":"lang"`), indexOf(string(body), `"name":"meta"`),
		"the encoded bytes carry the canonical order")
}
