package files_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/modality"
	"github.com/authzed/openagentprimitives/pkg/agent/modality/files"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

func TestFilesModalityTier1Surface(t *testing.T) {
	m := files.New()

	// Test Name()
	assert.Equal(t, "files", m.Name())

	// Test MetaTools with Reader present
	env := modality.Env{Reader: files.StoreReader{Store: blob.NewMem()}}
	tools := m.MetaTools(env)
	assert.NotEmpty(t, tools, "MetaTools should return tools when Reader is present")

	// Collect tool names
	toolNames := make([]string, len(tools))
	for i, tool := range tools {
		toolNames[i] = tool.Name()
	}

	// Verify fetch_artifact is present and mount_artifact is not
	assert.Contains(t, toolNames, "fetch_artifact", "MetaTools should include fetch_artifact")
	assert.NotContains(t, toolNames, "mount_artifact", "MetaTools should NOT include mount_artifact (Tier-2)")

	// Test MetaTools with nil Reader
	emptyEnv := modality.Env{Reader: nil}
	emptyTools := m.MetaTools(emptyEnv)
	assert.Empty(t, emptyTools, "MetaTools should be empty when Reader is nil")

	// Test Instructions
	instructions := m.Instructions(modality.Env{})
	assert.Contains(t, instructions, "fetch_artifact", "Instructions should mention fetch_artifact")
	assert.NotContains(t, instructions, "mount_artifact", "Tier-1 Instructions should NOT mention mount_artifact")
}

func TestFilesModalityTier2FilesOutSurface(t *testing.T) {
	m := files.New()

	// Files-out only (CapNativeFileOut, no Bridge): artifact_prepare's
	// container_file guidance appears, but mount_artifact (files-in) does not
	// — the runner has no artifactstore write access and no Bridge is wired.
	env := modality.Env{
		NativeOptIn: true,
		ModelCaps:   llm.NewCapabilitySet(llm.CapNativeFileOut),
		Reader:      files.StoreReader{Store: blob.NewMem()},
	}
	tools := m.MetaTools(env)

	toolNames := make([]string, len(tools))
	for i, tl := range tools {
		toolNames[i] = tl.Name()
	}
	assert.Contains(t, toolNames, "fetch_artifact", "Tier-2 files-out env should still offer fetch_artifact")
	assert.NotContains(t, toolNames, "mount_artifact", "files-out env with no Bridge should not offer mount_artifact")

	instructions := m.Instructions(env)
	assert.Contains(t, instructions, "container_file", "files-out Instructions should mention container_file")
	assert.NotContains(t, instructions, "mount_artifact", "files-out-only Instructions should NOT mention mount_artifact")
}

func TestFilesModalityTier2FilesInSurface(t *testing.T) {
	m := files.New()
	fb := &fakeBridge{intoContainerID: "file_out"}

	env := modality.Env{
		NativeOptIn: true,
		ModelCaps:   llm.NewCapabilitySet(llm.CapNativeFileIn),
		Reader:      files.StoreReader{Store: blob.NewMem()},
		Bridge:      fb,
	}
	tools := m.MetaTools(env)

	toolNames := make([]string, len(tools))
	for i, tl := range tools {
		toolNames[i] = tl.Name()
	}
	assert.Contains(t, toolNames, "fetch_artifact", "Tier-2 env should still offer fetch_artifact")
	assert.Contains(t, toolNames, "mount_artifact", "Tier-2 env should offer mount_artifact")

	instructions := m.Instructions(env)
	assert.Contains(t, instructions, "mount_artifact", "Tier-2 Instructions should mention mount_artifact")
	assert.NotContains(t, instructions, "container_file", "files-in-only Instructions should NOT mention container_file")
}

func TestFilesModalityNativeOptOutHasNoTier2(t *testing.T) {
	m := files.New()

	// ModelCaps advertises the capability, but NativeOptIn is false: Tier-2
	// must stay off (NativeActive requires both).
	env := modality.Env{
		NativeOptIn: false,
		ModelCaps:   llm.NewCapabilitySet(llm.CapNativeFileIn),
		Reader:      files.StoreReader{Store: blob.NewMem()},
		Bridge:      &fakeBridge{},
	}
	tools := m.MetaTools(env)
	toolNames := make([]string, len(tools))
	for i, tl := range tools {
		toolNames[i] = tl.Name()
	}
	assert.NotContains(t, toolNames, "mount_artifact", "opted-out native handling must not offer mount_artifact")
	assert.NotContains(t, m.Instructions(env), "mount_artifact", "opted-out native handling must not mention mount_artifact")
}
