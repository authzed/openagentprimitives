package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/artifactdelivery"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

// The delegated child's door out of a session is return_result, not
// agent_work_complete — the capability layer swaps one for the other on
// spec.parent. That swap replaces the DOOR; it must not replace the GATE.
//
// A completion requirement is the operator's guarantee about what a session of
// that class produces, and a child reporting to an agent rather than to a
// person does not void it — it is precisely the session nobody is watching.
// Without these, a class declaring `artifact-delivered` would hold for every
// session a human can see and silently not hold for the unattended ones.
//
// The fixtures (newGateSession, readyRender) are shared with
// agent_complete_requirements_test.go on purpose: the two tools are asserted
// against the SAME session shape, so a difference in outcome is a difference in
// the tool rather than in the setup.

func TestReturnResultGate_UnmetRequirementRefusesADelegatedChild(t *testing.T) {
	g := newGateSession(t, readyRender("ar-gate-1-report"))

	tl := meta.NewReturnResult(meta.CompletionConfig{
		Requirements: []string{artifactdelivery.Key},
		RecordBypass: func(context.Context, completion.Bypass) error { return nil },
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"result":"the translated line"}`), g.sess)
	require.NoError(t, err, "a refusal is a tool result the model can act on, not a Go error")

	assert.True(t, res.IsError, "an unmet requirement must refuse the child too")
	assert.False(t, res.Terminal, "a refused return must not end the session")
	assert.Nil(t, g.submitted, "a refused return must not hand the parent a result")
	assert.Contains(t, res.Content, artifactdelivery.Key, "the refusal must name what is missing")
	assert.Contains(t, res.Content, "bypass_reason", "the refusal must name the escape it offers")
	assert.Contains(t, res.Content, "return_result",
		"the retry instruction must name the tool this session actually has; agent_work_complete is not offered to a child")
	assert.NotContains(t, res.Content, "agent_work_complete",
		"naming the absent tool would point the child at an escape it cannot reach")
}

func TestReturnResultGate_ReasonedBypassReturnsAndIsRecorded(t *testing.T) {
	g := newGateSession(t, readyRender("ar-gate-1-report"))

	var got *completion.Bypass
	tl := meta.NewReturnResult(meta.CompletionConfig{
		Requirements: []string{artifactdelivery.Key},
		RecordBypass: func(_ context.Context, b completion.Bypass) error { got = &b; return nil },
	})

	args := json.RawMessage(`{"result":"the translated line","bypass_reason":"the render never came back; the translation itself is here"}`)
	res, err := tl.Execute(context.Background(), args, g.sess)
	require.NoError(t, err)

	assert.False(t, res.IsError, "content: %s", res.Content)
	assert.True(t, res.Terminal, "a reasoned bypass must let the child return")
	require.NotNil(t, g.submitted, "the answer must still reach the parent")
	assert.Contains(t, g.submitted.Summary, "translated line",
		"the bypass path must not swallow the deliverable")
	require.NotNil(t, got, "the bypass must be recorded, not merely allowed")
	require.Len(t, got.Unmet, 1, "the record must carry WHAT was skipped")
	assert.Equal(t, artifactdelivery.Key, got.Unmet[0].Key)
}

func TestReturnResultGate_UnrecordableBypassIsRefused(t *testing.T) {
	g := newGateSession(t, readyRender("ar-gate-1-report"))

	// No recorder: a child's bypass notice travels its own binding to the
	// delegating agent. An override nobody is told about is just an off switch,
	// and it is refused here exactly as it is on agent_work_complete.
	tl := meta.NewReturnResult(meta.CompletionConfig{
		Requirements: []string{artifactdelivery.Key},
	})

	args := json.RawMessage(`{"result":"the translated line","bypass_reason":"finishing anyway"}`)
	res, err := tl.Execute(context.Background(), args, g.sess)
	require.NoError(t, err)

	assert.True(t, res.IsError)
	assert.False(t, res.Terminal)
	assert.Nil(t, g.submitted)
	assert.Contains(t, res.Content, "cannot record")
}

func TestReturnResultGate_UndeclaredRequirementLeavesTheChildUngated(t *testing.T) {
	// The same session the refusal case uses: a Ready render nobody attached.
	// A class that declared nothing must still return — registering a
	// requirement makes it available, never on.
	g := newGateSession(t, readyRender("ar-gate-1-report"))

	tl := meta.NewReturnResult(meta.CompletionConfig{})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"result":"the translated line"}`), g.sess)
	require.NoError(t, err)

	assert.False(t, res.IsError, "content: %s", res.Content)
	assert.True(t, res.Terminal)
	require.NotNil(t, g.submitted)
	assert.Equal(t, "the translated line", g.submitted.Summary)
}

// TestReturnResultGate_BypassReasonOnlyOfferedWhenSomethingCanBeBypassed
// guards the schema, the same honest-schema rule agent_work_complete follows: a
// field the model can see is a field it will eventually try.
func TestReturnResultGate_BypassReasonOnlyOfferedWhenSomethingCanBeBypassed(t *testing.T) {
	props := func(t *testing.T, tl tool.Tool) map[string]any {
		t.Helper()
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema), "schema must be valid JSON")
		return schema.Properties
	}

	ungated := props(t, meta.NewReturnResult(meta.CompletionConfig{}))
	assert.NotContains(t, ungated, "bypass_reason", "a class with no requirements is offered no bypass")
	assert.Contains(t, ungated, "result", "the ungated schema is otherwise unchanged")

	gated := props(t, meta.NewReturnResult(meta.CompletionConfig{
		Requirements: []string{artifactdelivery.Key},
	}))
	assert.Contains(t, gated, "bypass_reason", "a class with requirements must be told how to override one")
	assert.Contains(t, gated, "result", "the gate adds a field; it does not replace the deliverable")
}

// TestBothTerminalToolsRunOneGate is the anti-drift claim, and it is the reason
// the gate is a shared type rather than a second copy: ONE config, both doors,
// the same verdict. A gate reimplemented on one side would pass every test
// above and still fail this one the day the two implementations diverge.
func TestBothTerminalToolsRunOneGate(t *testing.T) {
	cfg := meta.CompletionConfig{
		Requirements: []string{artifactdelivery.Key},
		RecordBypass: func(context.Context, completion.Bypass) error { return nil },
	}
	cases := []struct {
		name string
		tool tool.Tool
		args string
	}{
		{name: "agent_work_complete: refuses with the requirement unmet", tool: meta.NewAgentWorkComplete(cfg), args: `{"summary":"done"}`},
		{name: "return_result: refuses on the same session, for the same reason", tool: meta.NewReturnResult(cfg), args: `{"result":"the answer"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateSession(t, readyRender("ar-gate-1-report"))
			res, err := tc.tool.Execute(context.Background(), json.RawMessage(tc.args), g.sess)
			require.NoError(t, err)
			assert.True(t, res.IsError, "content: %s", res.Content)
			assert.False(t, res.Terminal)
			assert.Nil(t, g.submitted)
			assert.Contains(t, res.Content, artifactdelivery.Key)
		})
	}
}
