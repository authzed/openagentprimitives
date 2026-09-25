package meta

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
)

// The agent is the only party that can fix a plan whose handles were dropped,
// and update_plan's result is the one thing it is guaranteed to read. Logging
// the drop reaches an operator later; returning it reaches the author now.
func TestUpdatePlanResult_carriesPhaseNoticesToTheAgent(t *testing.T) {
	res, err := marshalUpdatePlanResultWithNotices(plans.UpdateResult{}, []string{
		`phase 0: permission "linear_list_projects" is not a valid handle`,
		"declarable handles on this session: perm:read:linear_issue",
	})
	require.NoError(t, err)

	assert.False(t, res.IsError,
		"the plan WAS stored and the phase is real, just narrower — an error would tell the agent to retry the whole call")
	assert.Contains(t, res.Content, "linear_list_projects")
	assert.Contains(t, res.Content, "perm:read:linear_issue")

	// The structured body must still parse: channels and the agent both read it.
	first := res.Content[:strings.IndexByte(res.Content, '\n')]
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(first), &body),
		"the JSON result must remain the first line and stay parseable")
}

func TestUpdatePlanResult_noNoticesLeavesTheResultUnchanged(t *testing.T) {
	plain, err := marshalUpdatePlanResult(plans.UpdateResult{})
	require.NoError(t, err)
	withNone, err := marshalUpdatePlanResultWithNotices(plans.UpdateResult{}, nil)
	require.NoError(t, err)

	assert.Equal(t, plain.Content, withNone.Content,
		"a correct plan must read exactly as before; noise on every call is noise the model learns to skip")
}

// The schema is the prevention half: the observed failure was an agent writing
// a raw tool name into `handle`, which the old schema ("type: string") did
// nothing to discourage.
func TestUpdatePlanSchema_describesTheHandleFormat(t *testing.T) {
	schema := string((&updatePlanTool{}).InputSchema())
	assert.Contains(t, schema, "perm:<permission>:<resourceType>")
	assert.Contains(t, schema, "NOT a tool name")
	assert.Contains(t, schema, "pattern",
		"a pattern makes a tool name structurally invalid rather than merely discouraged")
}

var _ = context.Background
