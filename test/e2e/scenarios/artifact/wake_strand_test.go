//go:build e2e

package artifact_test

import (
	"context"
	"github.com/authzed/openagentprimitives/test/e2e"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
)

// staleSessionListClient makes ONE AgentSession List return the phase the
// session had a moment earlier — a parked session reported as Running.
//
// That is not a contrivance: it is exactly the production window. channelsd's
// Deliver classifies from a List taken at its top, then does the interact
// Check, the memory append and the msg-ref index write before deciding whether
// the operator must respawn an exited runner. A runner calling
// agent_work_complete inside that window leaves the snapshot Running while the
// live object is already Idle. Injecting it here turns a probabilistic race
// (roughly 1 in 4 under load) into a deterministic test.
//
// Armed once and consumed only when it actually rewrites something, so the
// racing inbound is the one that sees it.
type staleSessionListClient struct {
	client.Client
	armed atomic.Bool
}

func (c *staleSessionListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	sessions, ok := list.(*spiceboxv1alpha1.AgentSessionList)
	if !ok || !c.armed.Load() {
		return nil
	}
	for i := range sessions.Items {
		if sessions.Items[i].Status.Phase != spiceboxv1alpha1.AgentSessionPhaseIdle {
			continue
		}
		sessions.Items[i].Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
		c.armed.Store(false)
	}
	return nil
}

// TestNoStrandedInbox_RunnerIdlesDuringDeliver is the end-to-end regression
// guard for the silently-stranded message.
//
// Before the fix, channelsd gated the wake annotation on Deliver's opening
// snapshot. With that snapshot reading Running for an already-Idle session, no
// annotation was written, delivery fell through to a NATS wakeup the exited
// runner never received, and the user's turn sat in memory as an undrained
// "inbox" entry — no reply, no error, no signal, until the user happened to
// send another message. On this build the test hangs at ExpectAgentReply and
// then fails the invariant; with the fix the fresh re-read inside annotateWake
// still sees Idle and the operator respawns.
func TestNoStrandedInbox_RunnerIdlesDuringDeliver(t *testing.T) {
	var stale *staleSessionListClient
	h := e2e.Start(t, e2e.Options{
		AgentDir: e2e.TestdataDir("agent-centerdot-companies"),
		PipelineExtender: func(pl *pipeline.Pipeline, v e2e.ExtenderView) {
			stale = &staleSessionListClient{Client: pl.K8s}
			pl.K8s = stale
		},
	})

	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	// Each turn: user text → respond_to_user → agent_work_complete (idles the
	// session, pod exits — the shape every centerdot multi-turn scenario has).
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()
	h.LLM.OnUserMessage("first question").Reply(e2e.RespondToUser("first answer")).Repeating()
	h.LLM.OnUserMessage("second question").Reply(e2e.RespondToUser("second answer")).Repeating()
	h.LLM.On(func(llm.Request) bool { return true }).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()

	h.SendUserMessage("first question")
	h.ExpectAgentReply(e2e.Contains("first answer"))
	e2e.WaitForSessionIdle(t, h)

	// The session is genuinely parked now. Arm the stale snapshot so the next
	// inbound is classified from the phase it had before the runner idled.
	stale.armed.Store(true)

	h.SendUserMessage("second question")
	h.ExpectAgentReply(e2e.Contains("second answer"))
	e2e.WaitForSessionIdle(t, h)

	// The property, independent of how we got here: nothing is left accepted
	// but unrunnable.
	h.AssertNoStrandedInbox()
}
