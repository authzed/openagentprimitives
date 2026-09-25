package meta_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func findAgentWorkComplete(t *testing.T) tool.Tool {
	t.Helper()
	for _, x := range meta.Load() {
		if x.Name() == "agent_work_complete" {
			return x
		}
	}
	t.Fatal("agent_work_complete not registered")
	return nil
}

func TestAgentWorkCompleteMetadata(t *testing.T) {
	t1 := findAgentWorkComplete(t)
	assert.Equal(t, tool.KindMeta, t1.Kind(), "Kind must be KindMeta")
	assert.NotEmpty(t, t1.Description(), "Description must be non-empty")

	var schema map[string]any
	require.NoError(t, json.Unmarshal(t1.InputSchema(), &schema), "schema must be valid JSON")

	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok, "schema.properties missing")
	_, hasSummary := props["summary"]
	assert.True(t, hasSummary, "schema must define summary")

	ap, ok := schema["additionalProperties"].(bool)
	assert.True(t, ok, "top-level additionalProperties must be a bool")
	assert.False(t, ap, "top-level additionalProperties must be false")

	artifacts, ok := props["artifacts"].(map[string]any)
	require.True(t, ok, "artifacts property missing or wrong type")
	items, ok := artifacts["items"].(map[string]any)
	require.True(t, ok, "artifacts.items missing")
	itemsAP, ok := items["additionalProperties"].(bool)
	assert.True(t, ok, "artifacts.items.additionalProperties must be a bool")
	assert.False(t, itemsAP, "artifacts.items.additionalProperties must be false")
}

// TestAgentWorkCompleteSummarySchemaPromisesNoDelivery guards the `summary`
// field's own description against re-acquiring a claim that it reaches the
// user. It does not: Execute hands the summary to SubmitResult, which records
// it on AgentSession.status for kubectl/audit and posts nothing. The model
// reads this description at argument-fill time, so a description promising
// delivery invites exactly the failure this tool cannot recover from — the
// round's actual answer stuffed into `summary`, no respond_to_user call, the
// session going Idle, and the user seeing silence while every layer reports
// success. The field description must instead route the model to
// respond_to_user, matching Description() and the runner's channel protocol.
func TestAgentWorkCompleteSummarySchemaPromisesNoDelivery(t *testing.T) {
	t1 := findAgentWorkComplete(t)

	var schema struct {
		Properties struct {
			Summary struct {
				Description string `json:"description"`
			} `json:"summary"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(t1.InputSchema(), &schema), "schema must be valid JSON")

	desc := schema.Properties.Summary.Description
	require.NotEmpty(t, desc, "summary property must carry a description")

	for _, forbidden := range []string{
		"shown verbatim to the user",
		"shown to the user",
		"displayed to the user",
		"delivered to the user",
	} {
		assert.NotContains(t, strings.ToLower(desc), forbidden,
			"summary description must not promise the summary reaches the user; it is audit-only")
	}
	assert.Contains(t, desc, "respond_to_user",
		"summary description must name respond_to_user as the way to reach the user")
}

func TestAgentWorkCompleteExecuteHappyPath(t *testing.T) {
	t1 := findAgentWorkComplete(t)
	got := tool.AgentResult{}
	sess := &tool.SessionContext{
		Namespace: "default", Name: "s1",
		SubmitResult: func(r tool.AgentResult) { got = r },
	}
	args := json.RawMessage(`{"summary":"all done","artifacts":[{"id":"abc","description":"diff"}]}`)
	res, err := t1.Execute(context.Background(), args, sess)
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.Terminal, "Result.Terminal must be true on completion")
	assert.True(t, res.Trusted, "agent_work_complete is a framework meta tool and must opt out of content-guard inspection")
	assert.Equal(t, "all done", got.Summary)
	require.Len(t, got.Artifacts, 1, "expected exactly one artifact")
	assert.Equal(t, "abc", got.Artifacts[0].ID)
}

func TestAgentWorkCompleteRejectsMissingSummary(t *testing.T) {
	t1 := findAgentWorkComplete(t)
	sess := &tool.SessionContext{SubmitResult: func(_ tool.AgentResult) {}}
	args := json.RawMessage(`{"artifacts":[]}`)
	res, _ := t1.Execute(context.Background(), args, sess)
	assert.True(t, res.IsError, "missing summary must produce IsError result")
	assert.False(t, res.Terminal, "validation failure must not terminate")
}
