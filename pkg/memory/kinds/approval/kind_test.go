package approval_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
)

func TestApproval_Registered(t *testing.T) {
	k, ok := memory.LookupKind("approval")
	require.True(t, ok)
	assert.Equal(t, "approval-", k.IDPrefix())
}

func TestApproval_RequestThenResolve(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, approval.RecordRequest(ctx, m, scope, "tu-1", approval.Request{
		ToolName: "github.create_repo", Approver: "alice@example.com",
	}))
	require.NoError(t, approval.RecordOutcome(ctx, m, scope, "tu-1", approval.Outcome{
		Decision: "approved", Approver: "alice@example.com",
	}))

	pair, err := approval.ByToolCall(ctx, m, scope, "tu-1")
	require.NoError(t, err)
	require.NotNil(t, pair.Request)
	require.NotNil(t, pair.Outcome)
	assert.Equal(t, "github.create_repo", pair.Request.ToolName)
	assert.Equal(t, "approved", pair.Outcome.Decision)
}

func TestApproval_RequestByID_RoundTripsRequest(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	details, err := json.Marshal(channelevents.ToolApprovalDetails{
		Permission:   "repo_write",
		ResourceType: "repo",
		ResourceID:   "org/x",
		ArgsJSON:     `{"title":"t"}`,
	})
	require.NoError(t, err, "marshal details")

	require.NoError(t, approval.RecordRequest(ctx, m, scope, "tu-1", approval.Request{
		RequestID: "req-42",
		ToolName:  "github_create_issue",
		Approver:  "user:alice",
		Details:   details,
	}), "RecordRequest tagged by RequestID")

	got, err := approval.RequestByID(ctx, m, scope, "req-42")
	require.NoError(t, err, "RequestByID")
	require.NotNil(t, got, "request must be found by request id")
	assert.Equal(t, "req-42", got.RequestID, "requestID round-trips")
	assert.Equal(t, "user:alice", got.Approver)

	var back channelevents.ToolApprovalDetails
	require.NoError(t, json.Unmarshal(got.Details, &back), "details must round-trip")
	assert.Equal(t, `{"title":"t"}`, back.ArgsJSON)
	assert.Equal(t, "repo", back.ResourceType)
}

func TestApproval_RequestByID_MissReturnsNilNil(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	got, err := approval.RequestByID(memory.WithSystemApproval(context.Background(), "test"), m, scope, "req-nope")
	require.NoError(t, err, "a miss is not an error")
	assert.Nil(t, got)

	got, err = approval.RequestByID(memory.WithSystemApproval(context.Background(), "test"), m, scope, "")
	require.NoError(t, err, "empty id is not an error")
	assert.Nil(t, got)
}
