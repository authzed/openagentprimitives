package slack

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// toolProgEnv builds a valid KindToolProgress envelope for the given payload.
func toolProgEnv(t *testing.T, pl channelevents.ToolProgressPayload) channelevents.Envelope {
	t.Helper()
	env, err := channelevents.BuildEnvelope("ns", "sess", channelevents.KindToolProgress, pl)
	require.NoError(t, err)
	return env
}

func TestSendToolProgress_SingleTool_AugmentsCaption(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{Namespace: "ns", Name: "sess"}
	// Seed the agent's cached caption as sendNotification would.
	s.rememberStatus(sess, cachedStatus{channelID: "C1", threadTS: "T1", status: "Cloning the repo", loading: "Cloning"})

	err := s.sendToolProgress(context.Background(), sess, "C1", "T1",
		toolProgEnv(t, channelevents.ToolProgressPayload{CallID: "tc1", Name: "git clone", BudgetSeconds: 300, ElapsedSeconds: 14}))
	require.NoError(t, err)

	require.NotEmpty(t, c.setStatusCalls, "expected at least one setStatus call")
	got := c.setStatusCalls[len(c.setStatusCalls)-1]
	assert.Contains(t, got.status, "Cloning the repo", "keeps the agent caption")
	assert.Contains(t, got.status, "git clone 14s", "appends the tool clause")
	assert.Contains(t, got.status, "/5m", "appends the budget ceiling")
}

func TestSendToolProgress_ParallelTools_Summarizes(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{Namespace: "ns", Name: "sess"}

	require.NoError(t, s.sendToolProgress(context.Background(), sess, "C1", "T1",
		toolProgEnv(t, channelevents.ToolProgressPayload{CallID: "tc1", Name: "git clone", ElapsedSeconds: 41})))
	require.NoError(t, s.sendToolProgress(context.Background(), sess, "C1", "T1",
		toolProgEnv(t, channelevents.ToolProgressPayload{CallID: "tc2", Name: "npm install", ElapsedSeconds: 8})))

	require.NotEmpty(t, c.setStatusCalls)
	got := c.setStatusCalls[len(c.setStatusCalls)-1]
	assert.Contains(t, got.status, "2 tools", "summarizes N parallel tools")
	assert.Contains(t, got.status, "git clone 41s", "names the longest-running")
}

func TestSendToolProgress_DoneRemovesAndRestores(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{Namespace: "ns", Name: "sess"}
	s.rememberStatus(sess, cachedStatus{channelID: "C1", threadTS: "T1", status: "Cloning the repo", loading: "Cloning"})

	require.NoError(t, s.sendToolProgress(context.Background(), sess, "C1", "T1",
		toolProgEnv(t, channelevents.ToolProgressPayload{CallID: "tc1", Name: "git clone", ElapsedSeconds: 14})))
	require.NoError(t, s.sendToolProgress(context.Background(), sess, "C1", "T1",
		toolProgEnv(t, channelevents.ToolProgressPayload{CallID: "tc1", Done: true})))

	require.NotEmpty(t, c.setStatusCalls)
	got := c.setStatusCalls[len(c.setStatusCalls)-1]
	assert.Equal(t, "Cloning the repo", got.status, "restores the plain caption when the set drains")
}

// TestSendToolProgress_DoneRestoresGenericWhenNothingCached covers the drain
// branch when the agent NEVER called update_status before the first tool tick:
// the finished tool's clause must NOT linger — the base caption is re-rendered
// with no clause, falling back to genericStatusCaption (matching how
// sendTurnProgress picks its base when nothing is cached).
func TestSendToolProgress_DoneRestoresGenericWhenNothingCached(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{Namespace: "ns", Name: "sess"}
	// No rememberStatus: the agent never issued an update_status caption.

	require.NoError(t, s.sendToolProgress(context.Background(), sess, "C1", "T1",
		toolProgEnv(t, channelevents.ToolProgressPayload{CallID: "tc1", Name: "git clone", ElapsedSeconds: 14})))
	require.NoError(t, s.sendToolProgress(context.Background(), sess, "C1", "T1",
		toolProgEnv(t, channelevents.ToolProgressPayload{CallID: "tc1", Done: true})))

	require.NotEmpty(t, c.setStatusCalls)
	got := c.setStatusCalls[len(c.setStatusCalls)-1]
	assert.Equal(t, genericStatusCaption, got.status, "drain re-renders the generic base, no lingering tool clause")
	assert.NotContains(t, got.status, "git clone", "the finished tool's clause must not remain")
}

// TestSendToolProgress_TouchesWatchdogEvenWhenSetStatusFails pins that the
// silence watchdog's liveness signal does not depend on Slack accepting a
// decorative status update.
//
// A tool_progress envelope IS forward progress: the runner emitted it. Slack's
// assistant.threads.setStatus, by contrast, rejects a non-assistant thread with
// invalid_thread_ts. Gating TouchSetStatus behind that call meant a healthy
// agent looked silent to the watchdog, which then posted "The agent appears to
// have stalled" every 120s — once per user message, forever.
func TestSendToolProgress_TouchesWatchdogEvenWhenSetStatusFails(t *testing.T) {
	c := &fakeSlackClient{setStatusErr: errors.New("invalid_thread_ts")}
	s := newSender(c)
	var touched []string
	s.deps.TouchSetStatus = func(ns, name string) { touched = append(touched, ns+"/"+name) }
	sess := channelkinds.SessionInfo{Namespace: "ns", Name: "sess"}

	require.NoError(t, s.sendToolProgress(context.Background(), sess, "C1", "T1",
		toolProgEnv(t, channelevents.ToolProgressPayload{CallID: "tc1", Name: "git clone", ElapsedSeconds: 14})),
		"a failed cosmetic setStatus stays best-effort")

	require.NotEmpty(t, c.setStatusCalls, "the sender still attempts the status update")
	assert.Equal(t, []string{"ns/sess"}, touched,
		"the runner made progress; the watchdog must be told even though Slack rejected the indicator")
}
