package browser

// threshold_test.go pins the browser tool package's local defaultMutateThreshold
// equal to haiku.DefaultMutateThreshold. The constant is duplicated (not imported)
// in the non-test build so this CDP/DOM package does not pull the LLM-provider
// transitive deps in just for a number; this test — which may carry the heavier
// dep — is the guard that the two never drift.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge/haiku"
)

func TestDefaultMutateThresholdMatchesHaiku(t *testing.T) {
	assert.Equal(t, haiku.DefaultMutateThreshold, defaultMutateThreshold,
		"the browser tool's local mutate threshold must stay equal to haiku.DefaultMutateThreshold (ruling E-R3)")
}
