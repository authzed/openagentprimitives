package manifests

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubstitute_registryPrefixesAllFirstParty(t *testing.T) {
	in := []byte("a spicebox-operator:dev\nb agentprimitives-channelsd:dev\nc pi-detector:dev\nd authzed/spicedb:latest")
	out, err := Substitute(in, Tags{Registry: "myreg.io/ap"})
	assert.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "myreg.io/ap/spicebox-operator:dev")
	assert.Contains(t, s, "myreg.io/ap/agentprimitives-channelsd:dev")
	assert.Contains(t, s, "myreg.io/ap/pi-detector:dev")
	assert.Contains(t, s, "authzed/spicedb:latest") // public image untouched
}

func TestSubstitute_explicitOverrideWinsOverRegistry(t *testing.T) {
	out, err := Substitute([]byte("x spicebox-operator:dev"), Tags{Registry: "myreg.io/ap", Operator: "ghcr.io/x/op:1.0"})
	assert.NoError(t, err)
	assert.Equal(t, "x ghcr.io/x/op:1.0", string(out))
}

func TestSubstitute_runnerOverride(t *testing.T) {
	out, _ := Substitute([]byte("r agentprimitives-runner:dev"), Tags{Runner: "ghcr.io/x/runner:2"})
	assert.Equal(t, "r ghcr.io/x/runner:2", string(out))
}

func TestSubstitute_noTagsIsNoOp(t *testing.T) {
	out, err := Substitute([]byte("spicebox-operator:dev"), Tags{})
	assert.NoError(t, err)
	assert.Equal(t, "spicebox-operator:dev", string(out))
}

func TestSubstitute_DigestPinsRemote(t *testing.T) {
	in := []byte("image: spicebox-operator:dev\nimage: agentprimitives-runner:dev\nimage: agentprimitives-webd:dev")
	out, err := Substitute(in, Tags{
		Registry: "myreg.io/ap",
		Digests: map[string]string{
			apimage.Operator.Name: "sha256:1111",
			apimage.Runner.Name:   "sha256:2222",
			// webd intentionally absent → registry tag
		},
	})
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "myreg.io/ap/spicebox-operator:dev@sha256:1111")
	assert.Contains(t, s, "myreg.io/ap/agentprimitives-runner:dev@sha256:2222")
	assert.Contains(t, s, "myreg.io/ap/agentprimitives-webd:dev") // no digest → tag
	assert.NotContains(t, s, "agentprimitives-webd:dev@")
}
