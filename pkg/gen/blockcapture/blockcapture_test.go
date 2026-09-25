package blockcapture

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func specimenByName(t *testing.T, name string) Specimen {
	t.Helper()
	specs, err := Specimens()
	require.NoError(t, err, "build specimens")
	for _, s := range specs {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no specimen %q", name)
	return Specimen{}
}

func captureBlocks(t *testing.T, name string) string {
	t.Helper()
	sp := specimenByName(t, name)
	posts, err := Capture(context.Background(), sp.SubChannel, sp.Session, sp.Envelopes...)
	require.NoError(t, err, "capture %q", name)
	blocks := pickBlocks(posts, sp.Pick)
	require.NotEmpty(t, blocks, "specimen %q produced no blocks (posts=%d)", name, len(posts))
	// Must be valid JSON (an array of blocks).
	var arr []map[string]any
	require.NoError(t, json.Unmarshal(blocks, &arr), "captured blocks are valid JSON array")
	require.NotEmpty(t, arr, "block array is non-empty")
	return string(blocks)
}

func TestApprovalCardRendersButtons(t *testing.T) {
	blocks := captureBlocks(t, "approval")
	// The real slack kind renders decision actions as buttons with our labels.
	assert.Contains(t, blocks, "\"actions\"", "approval should include an actions block")
	assert.Contains(t, blocks, "Approve")
	assert.Contains(t, blocks, "Deny")
	assert.Contains(t, blocks, "hotfix-1.4.2", "the lead text should survive into the blocks")
}

func TestResolvedApprovalKeepsDetailAndNamesApprover(t *testing.T) {
	blocks := captureBlocks(t, "approval-resolved")
	// The resolved card edits the original in place: it keeps the prompt detail…
	assert.Contains(t, blocks, "hotfix-1.4.2", "resolved card keeps the original prompt detail")
	// …and adds the verdict naming the approver as a native mention.
	assert.Contains(t, blocks, "U_OWNER", "resolved card names who decided")
	// The Approve/Deny action buttons are gone once resolved.
	assert.NotContains(t, blocks, "\"action_id\":\"approve\"", "resolved card drops the approve button")
}

func TestUserMessageRendersBlocks(t *testing.T) {
	blocks := captureBlocks(t, "message")
	assert.Contains(t, blocks, "Rollout complete", "message text should survive into the blocks")
}

func TestPlanUpdateRendersBlocks(t *testing.T) {
	blocks := captureBlocks(t, "plan")
	assert.Contains(t, blocks, "smoke suite", "plan item labels should survive into the blocks")
}

func TestGenerateWritesOneFilePerSpecimen(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Generate(dir))
	for _, name := range []string{"approval", "message", "plan"} {
		path := filepath.Join(dir, name+".json")
		data, err := os.ReadFile(path)
		require.NoError(t, err, "read %s", path)
		var arr []map[string]any
		require.NoError(t, json.Unmarshal(data, &arr), "%s is a valid Block Kit array", path)
		assert.NotEmpty(t, arr, "%s has blocks", path)
	}
}
