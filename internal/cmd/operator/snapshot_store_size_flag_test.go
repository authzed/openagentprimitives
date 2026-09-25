package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSnapshotStoreSizeFlagDefaultsToEmpty pins the flag default that makes the
// snapshot-store PVC follow the WORKSPACE size. snapshotStoreSize() falls back
// to workspaceSize() only when SnapshotStoreSize is "", so a non-empty flag
// default silently defeats that fallback — every session then costs
// workspace+8Gi of node-local disk instead of workspace+workspace, and enough
// of them drive the node-local (local-path) class into the disk-pressure
// deadlock snapshotStoreSize()'s fallback was written to prevent. The default
// MUST be empty; an operator that genuinely wants a fixed snapshot size still
// sets --snapshot-store-size explicitly.
func TestSnapshotStoreSizeFlagDefaultsToEmpty(t *testing.T) {
	cmd := newCommand()
	f := cmd.Flags().Lookup("snapshot-store-size")
	require.NotNil(t, f, "--snapshot-store-size must be a registered flag")
	assert.Equal(t, "", f.DefValue,
		"empty so snapshotStoreSize() falls back to the workspace size instead of a fixed 8Gi")
}
