//go:build e2e

package channel_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/test/e2e"
)

// channelHistoryEnabledOverride re-applies the fixture's channelhist-fake
// Channel with spec.channelHistory.enabled: true. Passed via
// e2e.Options.ExtraManifests, which applies AFTER AgentDir and updates an
// existing object in place (see Harness.applyManifestLabeled) — this is
// the "per-scenario override" contract ExtraManifests documents itself as.
const channelHistoryEnabledOverride = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: Channel
metadata:
  name: channelhist-fake
  namespace: default
spec:
  kind: fake
  role: both
  agentClass: channelhist-agent
  credentialsRef:
    secretName: channelhist-fake-creds
  fake:
    echo: false
  channelHistory:
    enabled: true
`

// TestChannelHistory_OptIn_RoundTrip proves the read_channel_history
// wiring end-to-end (Task 10): the runner offers the tool for a channel
// that opted in (spec.channelHistory.enabled), the tool's NATS request
// reaches historyresp.ChannelResponder (started alongside the harness's
// outbound relay in startChannelsdPlumbing), the responder derives the
// channel from the session, passes its gate (leakage off + output==input,
// both trivially true here — see the fixture's 01-agent.yaml), and reads
// via the fake kind's new ChannelHistoryReader implementation
// (pkg/channels/channelkinds/fake/channel_history.go) — and the agent receives the
// channel's canned messages ("alice: hello", "bob: world", ...) back as
// the tool result.
func TestChannelHistory_OptIn_RoundTrip(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-channel-history-e2e"),
		ExtraManifests: []string{channelHistoryEnabledOverride},
	})

	h.WaitForAgentClassValid("channelhist-agent", 30*time.Second)

	// LLM script: three rules for the full turn (mirrors the shape in
	// mcp_trust_annotations_test.go).
	//   1. User asks → tool_use for read_channel_history.
	//   2. read_channel_history tool_result (must carry the fake kind's
	//      canned messages) → respond_to_user.
	//   3. respond_to_user tool_result → EndTurn.
	//
	// Rule 1 is intentionally NOT .Repeating(): matchUserText walks
	// backward past tool-result-only messages to find the last actual text
	// block, so a Repeating rule here would keep re-matching (and
	// re-emitting the same tool_use) on every subsequent turn instead of
	// letting rule 2 take over once the tool_result lands.
	const userMsg = "what happened earlier"
	h.LLM.OnUserMessage(userMsg).
		Reply(e2e.ToolUse("read_channel_history", map[string]any{"limit": 10}))

	// matchToolResult strips the untrusted-tool-output wrapper and, since
	// the tool returns plain text (not JSON), the predicate receives it as
	// a raw string.
	var gotToolResult string
	h.LLM.OnToolResult("read_channel_history", func(v any) bool {
		s, ok := v.(string)
		if !ok {
			return false
		}
		gotToolResult = s
		return strings.Contains(s, "alice: hello") && strings.Contains(s, "bob: world")
	}).Reply(e2e.RespondToUser("Earlier: alice said hello, bob said world."))

	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendUserMessage(userMsg)
	got := h.ExpectAgentReply(e2e.Contains("alice said hello"))
	t.Logf("agent replied: %q", got.Text)

	require.NotEmpty(t, gotToolResult, "read_channel_history tool result was never observed by the scripted LLM")
	assert.Contains(t, gotToolResult, "alice: hello")
	assert.Contains(t, gotToolResult, "bob: world")

	h.AssertAllRulesConsumed()
}

// TestChannelHistory_NotOptedIn_ToolNotOffered is the negative row: a
// Channel that never set spec.channelHistory (the fixture's base state —
// no ExtraManifests override) must never see read_channel_history in its
// tool catalog. Proves channelhistorygate.Offer withholds the tool at
// offer-time, not just that the responder would withhold the read at
// request-time (see historyresp's TestHandle_GatingMatrix for that half).
func TestChannelHistory_NotOptedIn_ToolNotOffered(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: e2e.TestdataDir("agent-channel-history-e2e"),
	})

	h.WaitForAgentClassValid("channelhist-agent", 30*time.Second)

	// Not .Repeating(): see TestChannelHistory_OptIn_RoundTrip's rule-1
	// comment on why a Repeating OnUserMessage rule fights a later
	// OnToolResult rule for priority once a tool_result lands.
	h.LLM.OnUserMessage("hello").Reply(e2e.RespondToUser("hi there"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendUserMessage("hello")
	h.ExpectAgentReply(e2e.Contains("hi there"))
	h.AssertAllRulesConsumed()

	reqs := h.LLM.Requests()
	require.NotEmpty(t, reqs, "expected the scripted LLM to have observed at least one request")
	for _, req := range reqs {
		for _, tl := range req.Tools {
			assert.NotEqual(t, "read_channel_history", tl.Name,
				"read_channel_history must not be offered when spec.channelHistory is absent/disabled")
		}
	}
}
