package runner_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalactor"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// turnRefProbe records the loop's TurnRef each time the model calls it, the
// same moment a goals request made by a tool would read it.
type turnRefProbe struct {
	loop *runner.Loop
	seen []string
}

func (p *turnRefProbe) Name() string                 { return "probe_turn" }
func (p *turnRefProbe) Kind() tool.Kind              { return tool.KindMeta }
func (p *turnRefProbe) Description() string          { return "test-only: records the turn ref" }
func (p *turnRefProbe) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (p *turnRefProbe) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (p *turnRefProbe) PermissionVariants() []authz.PermissionVariant { return nil }
func (p *turnRefProbe) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	p.seen = append(p.seen, p.loop.TurnRef())
	return tool.Result{Content: "ok", Trusted: true}, nil
}

// attestedInboxWriter appends one authored, reffed inbox item mid-loop, as
// channelsd does for a message that arrives while the agent works.
type attestedInboxWriter struct {
	store  *turn.Appender
	author identity.Subject
	ref    string
	idx    int
}

func (w *attestedInboxWriter) Name() string                 { return "inject_attested" }
func (w *attestedInboxWriter) Kind() tool.Kind              { return tool.KindMeta }
func (w *attestedInboxWriter) Description() string          { return "test-only: writes an inbox turn" }
func (w *attestedInboxWriter) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (w *attestedInboxWriter) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (w *attestedInboxWriter) PermissionVariants() []authz.PermissionVariant { return nil }
func (w *attestedInboxWriter) Execute(ctx context.Context, _ json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	err := w.store.Append(ctx, memory.Turn{
		Index: w.idx, Role: "inbox", Author: w.author, InboundRef: w.ref,
		Content: []memory.ContentBlock{{Type: "text", Text: "another request"}},
	})
	return tool.Result{Content: "queued", Trusted: true}, err
}

var (
	turnAlice = identity.Subject("user:YWxpY2U")
	turnBob   = identity.Subject("user:Ym9i")
)

func awc(id string) llmfake.Step { return callStep(id, "agent_work_complete", `{"summary":"done"}`) }

func TestLoopTurnRef_ColdStartThenQueuedBehindWorkComplete(t *testing.T) {
	// probe (turn 0) → a message from bob lands mid-work → work complete with
	// it queued, which opens bob's turn → probe → work complete.
	l, _, _, store := newLoopFixture(t, []llmfake.Step{
		callStep("tu_1", "probe_turn", `{}`),
		callStep("tu_2", "inject_attested", `{}`),
		awc("tu_3"),
		callStep("tu_4", "probe_turn", `{}`),
		awc("tu_5"),
	})
	l.ChannelAttached = true
	probe := &turnRefProbe{loop: l}
	l.Tools = append(meta.Load(), probe, &attestedInboxWriter{store: store, author: turnBob, ref: "ref-b", idx: 99})

	ctx := memory.WithSystemApproval(context.Background(), "test")
	require.NoError(t, l.Run(ctx))
	assert.Equal(t, []string{goalactor.InitialPromptRef, "ref-b"}, probe.seen,
		"turn 0 is the opening prompt's; the inbound queued behind agent_work_complete opens the next turn")
}

// seedTranscript writes a resumed session's prior transcript: the opening
// prompt by alice, then an assistant turn calling lastTool, answered.
func seedTranscript(t *testing.T, store *turn.Appender, openerAuthor identity.Subject, lastTool string) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	require.NoError(t, store.Append(ctx, memory.Turn{Index: 0, Role: "user", Author: openerAuthor, Content: []memory.ContentBlock{{Type: "text", Text: "do the thing"}}}))
	require.NoError(t, store.Append(ctx, memory.Turn{Index: 1, Role: "assistant", Content: []memory.ContentBlock{{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "tu_prev", Name: lastTool, Input: []byte(`{}`)}}}}))
	require.NoError(t, store.Append(ctx, memory.Turn{Index: 2, Role: "user", Content: []memory.ContentBlock{{Type: "tool_result", ToolResult: &memory.ToolResultBlock{ToolUseID: "tu_prev", Content: "ok"}}}}))
}

func TestLoopTurnRef_ResumeAtRestVersusMidTurn(t *testing.T) {
	cases := []struct {
		name      string
		opener    identity.Subject
		lastTool  string
		inboxFrom identity.Subject
		inboxRef  string
		want      string
	}{
		{name: "at rest (agent_work_complete): the waking inbound opens the turn", opener: turnAlice, lastTool: "agent_work_complete", inboxFrom: turnBob, inboxRef: "ref-b", want: "ref-b"},
		{name: "mid-turn (await): another person's reply keeps the recovered opener's ref", opener: turnAlice, lastTool: "await_user_message", inboxFrom: turnBob, inboxRef: "ref-b", want: goalactor.InitialPromptRef},
		{name: "mid-turn (await): the opener's own reply advances to it", opener: turnAlice, lastTool: "await_user_message", inboxFrom: turnAlice, inboxRef: "ref-a2", want: "ref-a2"},
		{name: "mid-turn with no recoverable opener: unattributable", opener: "", lastTool: "await_user_message", inboxFrom: turnBob, inboxRef: "ref-b", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _, _, store := newLoopFixture(t, []llmfake.Step{
				callStep("tu_1", "probe_turn", `{}`),
				awc("tu_2"),
			})
			l.ChannelAttached = true
			probe := &turnRefProbe{loop: l}
			l.Tools = append(meta.Load(), probe)
			seedTranscript(t, store, tc.opener, tc.lastTool)
			ctx := memory.WithSystemApproval(context.Background(), "test")
			require.NoError(t, store.Append(ctx, memory.Turn{Index: 3, Role: "inbox", Author: tc.inboxFrom, InboundRef: tc.inboxRef, Content: []memory.ContentBlock{{Type: "text", Text: "reply"}}}))

			require.NoError(t, l.Run(ctx))
			assert.Equal(t, []string{tc.want}, probe.seen)
		})
	}
}

// A turn opened with no human in its opening batch (an authorless turn 0 — a
// kubectl, bento or trigger session) has no opener live, so a human who
// replies later never becomes one. A restart must not promote that replier.
func TestLoopTurnRef_ResumeNeverPromotesAMidTurnReplierToOpener(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "nothing queued: unattributable", true: "bob queued: unattributable"}[queued], func(t *testing.T) {
			l, _, _, store := newLoopFixture(t, []llmfake.Step{
				callStep("tu_1", "probe_turn", `{}`),
				awc("tu_2"),
			})
			l.ChannelAttached = true
			probe := &turnRefProbe{loop: l}
			l.Tools = append(meta.Load(), probe)
			seedTranscript(t, store, "", "await_user_message")
			ctx := memory.WithSystemApproval(context.Background(), "test")
			require.NoError(t, store.Append(ctx, memory.Turn{Index: 3, Role: "user", Author: turnAlice, InboundRef: "ref-a", Content: []memory.ContentBlock{{Type: "text", Text: "alice replies"}}}))
			if queued {
				require.NoError(t, store.Append(ctx, memory.Turn{Index: 4, Role: "inbox", Author: turnBob, InboundRef: "ref-b", Content: []memory.ContentBlock{{Type: "text", Text: "bob replies"}}}))
			}

			require.NoError(t, l.Run(ctx))
			assert.Equal(t, []string{""}, probe.seen)
		})
	}
}
