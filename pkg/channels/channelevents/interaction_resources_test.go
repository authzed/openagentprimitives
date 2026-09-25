package channelevents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInteractionRequestPayload_Resources_Optional(t *testing.T) {
	base := InteractionRequestPayload{
		Category: "tool_approval", RequestRef: "r1", Lead: "approve?",
		Audience: InteractionAudience{Scope: AudienceApprovers, Approvers: []ExternalIdentity{{Subject: "user:owner@corp.example"}}},
	}
	require.NoError(t, base.Validate())
	withRes := base
	withRes.Resources = []InteractionResourceRef{{Type: "data", ID: "d1"}}
	require.NoError(t, withRes.Validate())
	assert.Equal(t, "data", withRes.Resources[0].Type)
}
