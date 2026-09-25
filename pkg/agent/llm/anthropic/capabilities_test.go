package anthropic

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

func TestCapabilities_KnownAndUnknown(t *testing.T) {
	var p Provider
	assert.True(t, p.Capabilities("claude-opus-4-8").Has(llm.CapNativeFileOut),
		"opus 4.8 supports native file output")
	assert.Empty(t, p.Capabilities("claude-haiku-4-5"),
		"haiku 4.5 has no native file capabilities")
}
