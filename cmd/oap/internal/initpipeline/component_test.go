package initpipeline_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/initpipeline"
)

func TestRegistry_RegisterAndAll(t *testing.T) {
	initpipeline.ResetForTest()
	initpipeline.Register(initpipeline.Component{Name: "postgres", Inputs: []initpipeline.Input{{Flag: "x", Default: "d"}}})
	initpipeline.Register(initpipeline.Component{Name: "neo4j"})
	got := initpipeline.All()
	require.Len(t, got, 2)
	assert.Equal(t, "postgres", got[0].Name)
	assert.Equal(t, "neo4j", got[1].Name)
}
