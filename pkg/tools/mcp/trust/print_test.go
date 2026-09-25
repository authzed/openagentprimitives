package trust_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	mcpprobe "github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/trust"
)

func TestFromProbe(t *testing.T) {
	assert.Equal(t, "", trust.FromProbe(mcpprobe.Annotations{}))
	assert.Equal(t, "maliciousActivityHint=true",
		trust.FromProbe(mcpprobe.Annotations{MaliciousActivityHint: true}))
	assert.Equal(t, "inputMetadata=<see tool JSON>",
		trust.FromProbe(mcpprobe.Annotations{InputMetadata: json.RawMessage(`{"x":1}`)}))
	assert.Equal(t, "",
		trust.FromProbe(mcpprobe.Annotations{InputMetadata: json.RawMessage(`{}`)}))
}

func TestFromSpec(t *testing.T) {
	assert.Equal(t, "", trust.FromSpec(mcpspec.Trust{}))
	assert.Equal(t, "attribution=[mcp://a]",
		trust.FromSpec(mcpspec.Trust{Attribution: []string{"mcp://a"}}))
	assert.Equal(t, "returnMetadata=<see tool JSON>",
		trust.FromSpec(mcpspec.Trust{ReturnMetadata: json.RawMessage(`{"source":"internal"}`)}))
}
