package toolkit_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

func TestToolkit_StreamFormat_RoundTrip(t *testing.T) {
	in := toolkit.Toolkit{
		Name: "claude", Version: "1", ToolkitRevision: "x",
		Target:       toolkit.Target{Binary: "claude"},
		Parser:       toolkit.ParserConfig{Kind: "declarative"},
		StreamFormat: "claude-stream-json",
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"streamFormat":"claude-stream-json"`)

	var out toolkit.Toolkit
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, "claude-stream-json", out.StreamFormat)
}

func TestToolkit_StreamFormat_OmittedWhenEmpty(t *testing.T) {
	in := toolkit.Toolkit{Name: "git", Version: "1", ToolkitRevision: "x",
		Target: toolkit.Target{Binary: "git"},
		Parser: toolkit.ParserConfig{Kind: "declarative"},
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "streamFormat")
}
