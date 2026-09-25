package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

func TestSyncWorkspace_Metadata(t *testing.T) {
	tl := NewSyncWorkspace("git", "https://example.invalid/repo.git", "main", "/workspace", "overlay-pvc", "", "")
	assert.Equal(t, "sync_workspace", tl.Name())
	assert.NotEmpty(t, tl.Description())
	assert.NotEmpty(t, tl.InputSchema())
	assert.Nil(t, tl.PermissionVariants())
}

// TestSyncWorkspace_Passthrough pins the fix for the Phase-4 regression: sync
// must declare StateImpact=Passthrough (not Readonly) so it carries no
// Check and CheckRequired() is false, exempting it from the runner's
// PreToolCall authz gate. Under Readonly, CheckRequired() is true but sync
// never supplies a Check, so every call was denied fail-closed ("stateImpact
// requires a check but none was supplied") under the default enforcing
// toolAuthMode — sync only pulls the source's latest revision into the
// session's own overlay, so no per-call authz/approval decision applies.
func TestSyncWorkspace_Passthrough(t *testing.T) {
	tl := NewSyncWorkspace("git", "https://example.invalid/repo.git", "main", "/workspace", "overlay-pvc", "", "")
	assert.Equal(t, authz.Passthrough, tl.Permission().StateImpact)
	assert.False(t, tl.Permission().StateImpact.CheckRequired())
}

func TestApplyWorkspace_Metadata(t *testing.T) {
	tl := NewApplyWorkspace("git", "https://example.invalid/repo.git", "main", "/workspace", "overlay-pvc", "ws-cred", "", "")
	assert.Equal(t, "apply_workspace", tl.Name())
	assert.NotEmpty(t, tl.Description())
	assert.NotEmpty(t, tl.InputSchema())
	assert.Nil(t, tl.PermissionVariants())
}

// TestApplyWorkspace_External is the security invariant this task exists to
// enforce: apply_workspace must declare StateImpact=External so the runner's
// authz hook (pkg/authz/spicedb/toolcheck/check_tool_call.go) ALWAYS routes it through human
// approval before Execute ever runs — a compromised or misled agent can never
// silently push overlay edits back to the source origin. Unlike sync_workspace
// (Passthrough, exempt from the gate), apply's CheckRequired() must stay true
// so it's caught by the dispatch gate and forced through approval.
func TestApplyWorkspace_External(t *testing.T) {
	tl := NewApplyWorkspace("git", "https://example.invalid/repo.git", "main", "/workspace", "overlay-pvc", "ws-cred", "", "")
	assert.Equal(t, authz.External, tl.Permission().StateImpact)
	assert.True(t, tl.Permission().StateImpact.CheckRequired())
}
