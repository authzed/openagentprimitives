// pkg/agent/runner/speakerprofile_coldstart_test.go
//
// TestColdStartFirstTurnGetsSpeakerProfileBlock proves the fix for a
// functional gap the Task 6 re-review surfaced: lastInboundAuthor was only
// ever written by drainInbox, but a session's turn-0 initial prompt is
// placed by Run's cold-start path (loop.go, the block that builds
// memory.Turn{Index: 0, ..., Author: l.authorForStarter()}), which never
// passes through drainInbox. Without threading that Author into
// lastInboundAuthor too, the session's very first human turn never got a
// profile block — the capability effectively started at turn 2, defeating
// the common one-shot-question case.
//
// This lives in package runner_test (reusing loop_test.go's newLoopFixture)
// rather than package runner's speakerprofile_test.go, because it drives the
// full Run() cold-start path end-to-end through a scripted fake provider —
// an integration-shaped test, not a unit test of hydrateSpeakerProfile in
// isolation.
package runner_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
	"github.com/authzed/openagentprimitives/pkg/authz/untrusted"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// reqMsgText concatenates a request message's text blocks for assertion.
func reqMsgText(m llm.Message) string {
	var out string
	for _, c := range m.Content {
		out += c.Text
	}
	return out
}

// oneShotWorkComplete is a single scripted LLM response that ends the
// session in exactly one provider request — the smallest fixture that lets
// us pin "the session's first turn" to "the request's first message" without
// any further round-trips complicating the assertion.
func oneShotWorkComplete() llm.Response {
	return llm.Response{
		Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID: "tu_1", Name: "agent_work_complete",
				Input: json.RawMessage(`{"summary":"done"}`),
			}},
		},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 50},
	}
}

func TestColdStartFirstTurnGetsSpeakerProfileBlock(t *testing.T) {
	l, provider, _, _ := newLoopFixture(t, []llmfake.Step{{Resp: oneShotWorkComplete()}})

	// StartedByCanonical is what authorForStarter() turns into the turn-0
	// Author — the same canonical id production derives from the session
	// starter's verified identity (see internal/cmd/runner/main.go's
	// startedByCanonical()).
	canon, err := identity.VerifiedEmail(identity.Email("dana@example.com"), "").Canonical()
	require.NoError(t, err)
	l.StartedByCanonical = canon

	var calls int
	l.FetchSpeakerProfile = func(_ context.Context, email string) (userprofile.Profile, error) {
		calls++
		assert.Equal(t, "dana@example.com", email)
		return userprofile.Profile{Title: "Director of Support"}, nil
	}
	l.SpeakerProfileFields = userprofile.DefaultFields()

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	assert.Equal(t, 1, calls, "the fetcher must be called exactly once for the session's very first turn")

	reqs := provider.Requests()
	require.Len(t, reqs, 1, "the one-shot agent_work_complete script makes exactly one provider request")
	require.NotEmpty(t, reqs[0].Messages, "the request must carry the cold-start turn")
	firstTurn := reqMsgText(reqs[0].Messages[0])
	assert.Contains(t, firstTurn, untrusted.ProfileTag,
		"the session's FIRST human turn (the cold-start placement, which never passes through drainInbox) must carry the speaker profile block")
	assert.Contains(t, firstTurn, "Director of Support")
}

// TestColdStartNoStarterIdentityStaysInert proves the guard: a
// kubectl/bento-driven session with no human starter (StartedByCanonical
// empty, authorForStarter() == "") must stay exactly as inert as before —
// no fetch, no block — mirroring drainInbox's own `if !it.Author.Empty()`
// guard.
func TestColdStartNoStarterIdentityStaysInert(t *testing.T) {
	l, provider, _, _ := newLoopFixture(t, []llmfake.Step{{Resp: oneShotWorkComplete()}})
	// l.StartedByCanonical left zero-value ("") — no human starter.

	var calls int
	l.FetchSpeakerProfile = func(_ context.Context, email string) (userprofile.Profile, error) {
		calls++
		return userprofile.Profile{Title: "Director of Support"}, nil
	}
	l.SpeakerProfileFields = userprofile.DefaultFields()

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	assert.Zero(t, calls, "no starter identity means no speaker to fetch a profile for")
	reqs := provider.Requests()
	require.Len(t, reqs, 1)
	require.NotEmpty(t, reqs[0].Messages)
	assert.NotContains(t, reqMsgText(reqs[0].Messages[0]), untrusted.ProfileTag)
}
