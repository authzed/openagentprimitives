package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

// TestReturnResult_TellsTheChildTheFieldIsItsAnswer pins the prompt content,
// because for a tool that is what acts. A delegated child's returned string is
// copied verbatim into SubagentRequest.Status.Result and handed to its caller;
// a child told the field is an audit note writes "translated the line" and its
// caller receives that instead of the work. Observed live, nine turns of
// re-delegation.
func TestReturnResult_TellsTheChildTheFieldIsItsAnswer(t *testing.T) {
	rr := meta.NewReturnResult(meta.CompletionConfig{})

	assert.Equal(t, "return_result", rr.Name(),
		"the NAME is the strongest prompt this tool has; 'complete' framing is what it exists to avoid")

	desc := rr.Description()
	assert.Contains(t, desc, "verbatim")
	assert.Contains(t, desc, "ask_parent",
		"a child that cannot finish must be pointed somewhere other than a partial answer")
	// Not the mere mention — the description legitimately says there IS no
	// respond_to_user here. What must be absent is the INSTRUCTION to use one,
	// which is the sentence agent_work_complete carries and the child obeyed.
	assert.NotContains(t, desc, "via respond_to_user")
	assert.NotContains(t, desc, "respond_to_user FIRST",
		"routing a child to a tool it does not have is the original bug")

	var schema map[string]any
	require.NoError(t, json.Unmarshal(rr.InputSchema(), &schema))
	props, _ := schema["properties"].(map[string]any)
	require.Contains(t, props, "result", "the argument is `result`, not `summary` — the name is prompt too")
	require.NotContains(t, props, "summary",
		"calling it summary to the model is what produced summaries instead of answers")

	sd, _ := props["result"].(map[string]any)["description"].(string)
	assert.Contains(t, sd, "verbatim")
	assert.NotContains(t, sd, "audit")
}

// TestReturnResult_WritesTheAnswerIntoTheFieldTheParentReads is the half that
// makes the prompt true: `result` must land in AgentResult.Summary, because
// that is the field reconcileChild copies into SubagentRequest.Status.Result.
// A rename of the argument that did not carry through to here would leave the
// tool honest-sounding and the caller receiving nothing.
func TestReturnResult_WritesTheAnswerIntoTheFieldTheParentReads(t *testing.T) {
	rr := meta.NewReturnResult(meta.CompletionConfig{})
	var got tool.AgentResult
	sess := &tool.SessionContext{
		Namespace: "default", Name: "child-1",
		SubmitResult: func(r tool.AgentResult) { got = r },
	}

	args := json.RawMessage(`{"result":"Ze treasure be buried under ze old oak tree. One... two... ah-ah-ah!","artifacts":[{"id":"a1","description":"diff"}]}`)
	res, err := rr.Execute(context.Background(), args, sess)
	require.NoError(t, err)

	assert.True(t, res.Terminal,
		"the runner keys termination on Result.Terminal, not on a tool name — without this the child never ends")
	assert.True(t, res.Trusted, "a framework meta tool opts out of content-guard inspection")
	assert.Contains(t, got.Summary, "ah-ah-ah",
		"the answer must reach AgentResult.Summary, which is what the parent actually reads")
	require.Len(t, got.Artifacts, 1)
	assert.Equal(t, "a1", got.Artifacts[0].ID)
}

func TestReturnResult_RejectsAnEmptyAnswer(t *testing.T) {
	rr := meta.NewReturnResult(meta.CompletionConfig{})
	sess := &tool.SessionContext{SubmitResult: func(tool.AgentResult) {}}
	res, err := rr.Execute(context.Background(), json.RawMessage(`{"artifacts":[]}`), sess)
	require.NoError(t, err)
	assert.True(t, res.IsError, "an empty answer tells the caller the work produced nothing")
	assert.False(t, res.Terminal, "a validation failure must not end the session")
}
