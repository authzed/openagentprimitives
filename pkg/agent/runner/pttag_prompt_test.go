package runner_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
)

func TestComposeSystem_fineGrainedBlock(t *testing.T) {
	on := runner.ComposeSystem("p", nil, nil, nil, nil, nil, runner.WithFineGrainedInfoLeakage(true))
	assert.Contains(t, on, "pt-untrusted", "teaches the envelope")
	assert.Contains(t, on, "derive_tag", "teaches the derive behavior")
	assert.Contains(t, on, "verbatim", "tells the model to preserve the whole region")

	off := runner.ComposeSystem("p", nil, nil, nil, nil, nil)
	assert.NotContains(t, off, "pt-untrusted", "the block is opt-in — absent by default")
}
