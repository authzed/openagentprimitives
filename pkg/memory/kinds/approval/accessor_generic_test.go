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

func TestRecordRequest_GenericRoundTrip(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Details is json.RawMessage — a raw literal here; channelevents.ToolApprovalDetails
	// is not defined yet, so this test must not forward-reference it.
	det := json.RawMessage(`{"permission":"write","resourceType":"repo","resourceID":"r1"}`)
	require.NoError(t, approval.RecordRequest(ctx, m, scope, "tu-generic-1", approval.Request{
		RequestID: "req-1", ToolName: "git_push",
		Resources: []channelevents.InteractionResourceRef{{Type: "repo", ID: "r1"}},
		Details:   det,
	}))

	got, err := approval.RequestByID(ctx, m, scope, "req-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "req-1", got.RequestID)
	require.Len(t, got.Resources, 1)
	assert.Equal(t, "repo", got.Resources[0].Type)
	assert.Equal(t, "r1", got.Resources[0].ID)
	assert.NotEmpty(t, got.Details)
}
