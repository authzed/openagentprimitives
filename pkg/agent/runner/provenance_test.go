package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestToolProvenance_SetGetMissing(t *testing.T) {
	p := NewToolProvenance()
	require.NotNil(t, p)

	// Missing: unknown tool returns zero record + false.
	got, ok := p.Get("nonexistent_tool")
	assert.False(t, ok)
	assert.Equal(t, ProvenanceRecord{}, got)

	// Set a record and Get it back.
	rec := ProvenanceRecord{
		Pin: spiceboxv1alpha1.PinRecord{
			Kind:     "mcp",
			Strength: "frozen",
			Digest:   "sha256:abc123",
		},
		Name:         "my-server",
		DriftSummary: "",
		BypassReason: "",
	}
	p.Set("my_server_tool", rec)

	got, ok = p.Get("my_server_tool")
	require.True(t, ok)
	assert.Equal(t, rec, got)

	// Missing still false after another key was added.
	_, ok = p.Get("other_tool")
	assert.False(t, ok)
}

func TestToolProvenance_SetOverwrites(t *testing.T) {
	p := NewToolProvenance()

	first := ProvenanceRecord{
		Pin:  spiceboxv1alpha1.PinRecord{Kind: "mcp", Digest: "sha256:old"},
		Name: "server-a",
	}
	second := ProvenanceRecord{
		Pin:  spiceboxv1alpha1.PinRecord{Kind: "mcp", Digest: "sha256:new"},
		Name: "server-b",
	}

	p.Set("tool", first)
	p.Set("tool", second)

	got, ok := p.Get("tool")
	require.True(t, ok)
	assert.Equal(t, second, got, "second Set must overwrite first")
}

func TestToolProvenance_DriftedRecord(t *testing.T) {
	p := NewToolProvenance()

	rec := ProvenanceRecord{
		Pin: spiceboxv1alpha1.PinRecord{
			Kind:     "mcp",
			Strength: "named",
			Digest:   "sha256:live",
		},
		Name:         "drifted-server",
		DriftSummary: "MCPServer/drifted-server tools/list drifted (sha256:old -> sha256:live)",
		BypassReason: "",
	}
	p.Set("drifted_server_list", rec)

	got, ok := p.Get("drifted_server_list")
	require.True(t, ok)
	assert.Equal(t, rec.DriftSummary, got.DriftSummary)
	assert.NotEmpty(t, got.DriftSummary)
}
