//go:build e2e

package artifact_test

import (
	"context"
	"github.com/authzed/openagentprimitives/test/e2e"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	artifactkind "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
	"github.com/authzed/openagentprimitives/pkg/memory/spicedbauthorizer"
)

// TestArtifactViewAuthzChain exercises the artifact live-view authorization
// chain end to end against the real SpiceDB + composed schema. It is the
// coverage that was missing — and whose absence let a silent-drop bug ship:
//
//   - The agent runner persists artifacts as a SYSTEM caller. The memory
//     authorizer MUST write the caller-INDEPENDENT artifact#parent edge for
//     that write, because artifact#view = parent->interact gates DOWNSTREAM
//     session participants, not the writer. The old AuthorizePut silently
//     returned nil for system/no callers, so the edge was never written and
//     every live-view artifact 403'd ("you do not have access") with zero
//     diagnostics.
//
// The e2e harness historically constructed its memStore WITHOUT the authorizer
// (harness.go), so AuthorizePut's artifact branch never ran in e2e. This test
// drives the real authorizer against the harness's real SpiceDB so a
// regression of the silent-drop fix fails here.
//
// The AgentDir is applied only to trigger Guardian's schema composition (the
// embedded base schema carries `agentsession` + `artifact`); the agent itself
// is never driven.
func TestArtifactViewAuthzChain(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-leakage-e2e"),
		DefaultTimeout: 30 * time.Second,
	})
	h.MCP.OnTool("get_record", func(_ map[string]any) any { return map[string]any{"id": "x"} })
	h.MCP.OnTool("get_record_uuid", func(_ map[string]any) any { return map[string]any{"uuid": "u"} })
	h.MCP.OnTool("unmapped_tool", func(_ map[string]any) any { return map[string]any{"ok": true} })
	h.WaitForAgentClassValid("leakage-e2e", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	const (
		ns         = "default"
		sessName   = "artifact-view-authz"
		sessionID  = ns + "/" + sessName
		artifactID = "artifact-e2eviewchain"
	)
	// Canonical SpiceDB subjects (base64 of the email) — never raw emails;
	// SpiceDB object-ids reject @/. (the bug that 403'd with a 500 earlier).
	participant := e2e.CanonicalForFakeEmail("alice@example.com")
	stranger := e2e.CanonicalForFakeEmail("mallory@example.com")

	// A session whose owner is the participant. agentsession#interact =
	// owner + participant - denied, so the participant has interact.
	e2e.WriteRel(t, h, "agentsession", sessionID, "owner", "user", participant.String(), "")

	// Before the artifact's parent edge exists, even the participant cannot
	// view it — proves the later true is real, not trivially passing.
	h.AssertSpiceDB("artifact:"+artifactID, "view", participant.Subject(), false)

	// Persist the artifact through the REAL authorizer as the SYSTEM caller the
	// runner uses. On the buggy authorizer this returned nil and wrote nothing;
	// the fix writes artifact#parent regardless of caller.
	az := spicedbauthorizer.New(h.SpiceDB)
	err := az.AuthorizePut(
		memory.WithCaller(context.Background(), "system:channelsd"),
		memory.Entry{
			Scope: memory.Scope{Kind: "session", ID: sessionID},
			Kind:  artifactkind.KindName,
			ID:    artifactID,
		})
	require.NoError(t, err, "authorizer must write artifact#parent for a system caller")

	// The participant can now view; a stranger still cannot.
	h.AssertSpiceDB("artifact:"+artifactID, "view", participant.Subject(), true)
	h.AssertSpiceDB("artifact:"+artifactID, "view", stranger.Subject(), false)

	// And webd's exact gate (CheckArtifactView, the method internal/cmd/webd calls)
	// resolves the same way for the participant.
	ok, err := h.SpiceDB.CheckArtifactView(context.Background(), artifactID, participant, true)
	require.NoError(t, err)
	assert.True(t, ok, "CheckArtifactView (webd's live-view gate) must allow the participant")
}
