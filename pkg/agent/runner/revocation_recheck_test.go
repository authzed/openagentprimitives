package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/toolorigin"
)

// The revocation guard is a PreToolCall hook, and an approval await sits
// between that hook and the run. So an operator revoking an origin while a
// human was looking at the approval card had their revocation land, be
// recorded, and then be ignored by the very call it was aimed at — a window
// bounded only by the approval timeout, which is hours.
//
// executeToolContained now re-asks immediately before the call runs.
// originRevokedNow is that question, and it is a live read of an in-process
// set, so re-asking costs nothing and closes the whole gap rather than only the
// approval leg: anything that delays a call between the gates and the run is
// covered by the same line.

// revocationLoop wires a Loop whose tool set is one origin-bearing tool, since
// originRevokedNow resolves the origin through the live tool list.
func revocationLoop(t *testing.T, tools ...tool.Tool) *Loop {
	t.Helper()
	return &Loop{RevokedOrigins: toolorigin.New(), Tools: tools}
}

func TestOriginRevokedNow_ReportsARevocationThatLandedAfterTheGates(t *testing.T) {
	l := revocationLoop(t, shadowTool{name: "gh_list", kind: tool.KindMCP, origin: "mcpserver/gh"})

	origin, revoked := l.originRevokedNow("gh_list")
	require.False(t, revoked, "nothing is revoked yet — this is the state at PreToolCall")

	// The operator revokes while the approval card is still on someone's screen.
	require.NoError(t, l.RevokedOrigins.Invalidate("mcpserver/gh"))

	origin, revoked = l.originRevokedNow("gh_list")
	assert.True(t, revoked, "a revocation landing during the await must stop the call it targeted")
	assert.Equal(t, "mcpserver/gh", origin, "the refusal names the origin, so the model is told what happened")
}

func TestOriginRevokedNow_UnrevokedOriginRuns(t *testing.T) {
	l := revocationLoop(t, shadowTool{name: "gh_list", kind: tool.KindMCP, origin: "mcpserver/gh"})
	require.NoError(t, l.RevokedOrigins.Invalidate("mcpserver/other"))

	_, revoked := l.originRevokedNow("gh_list")
	assert.False(t, revoked, "revoking one origin must not stop another's tools")
}

// A meta or sandbox tool belongs to no upstream, so there is no origin to
// revoke — the same answer the guard itself gives. Reading "no origin" as
// "revoked" would refuse respond_to_user.
func TestOriginRevokedNow_ToolWithNoOrigin(t *testing.T) {
	l := revocationLoop(t, shadowTool{name: "respond_to_user", kind: tool.KindMeta})

	_, revoked := l.originRevokedNow("respond_to_user")
	assert.False(t, revoked)
}

// Revocation unwired is the pre-feature deployment, and it must behave exactly
// as it did — never as "everything is revoked".
func TestOriginRevokedNow_UnwiredIsNotRevoked(t *testing.T) {
	l := &Loop{}

	_, revoked := l.originRevokedNow("gh_list")
	assert.False(t, revoked)
}
