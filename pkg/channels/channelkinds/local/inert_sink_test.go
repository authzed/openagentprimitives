// The other half of inert_test.go: where that file pins WHAT the sweep keeps
// and drops, this one pins WHERE it runs — at the kind's single Emit door — and
// covers the render events whose payloads reach the terminal without any sender
// naming them field by field.
//
// The last test here is what makes an enumeration of sinks unnecessary: it
// emits a shape this package has never defined and requires the door to sweep
// it anyway. See NOTES.md.
package local

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// TestMainSender_OperationActivityIsInert is the same string, on the other
// route. sender.go sweeps the update_status caption because it is drawn on a
// line that stays visible while a decision modal is open — and the operation
// snapshot lands on that exact same status line, via
// OperationActivityPayload.CompactLine, which the channelsd watchdog fills
// from the notification's own Text on a cleared tick.
func TestMainSender_OperationActivityIsInert(t *testing.T) {
	sink := &RecordingSink{}
	s := &localSender{sink: newInertSink(sink)}
	_, err := s.Send(context.Background(), sessInfo(),
		mustEnv(t, channelevents.KindOperationActivity, channelevents.OperationActivityPayload{
			CompactLine: "Deploying ‣ " + cursorRepaint + "[a] Approve   [d] Deny",
			Operations: []channelevents.OperationActivityNode{{
				ID: "op-1", Description: "Deploy" + cursorRepaint,
				Calls: []channelevents.OperationActivityCallNode{
					{Tool: "kubectl" + oscTitle, Reason: "roll out" + cursorRepaint},
				},
			}},
		}))
	require.NoError(t, err, "Send")

	require.Len(t, sink.Events(), 1)
	m, ok := sink.Events()[0].(MsgOperationActivity)
	require.True(t, ok, "want MsgOperationActivity, got %T", sink.Events()[0])

	assert.NotContains(t, m.Payload.CompactLine, "\x1b",
		"CompactLine is drawn on the always-visible status line, the same line the swept notification caption uses")
	assert.NotContains(t, m.Payload.Operations[0].Description, "\x1b",
		"an operation description is the agent's own text")
	assert.NotContains(t, m.Payload.Operations[0].Calls[0].Tool, "\x1b")
	assert.NotContains(t, m.Payload.Operations[0].Calls[0].Reason, "\x1b",
		"a per-call _reason is the model's own sentence")
	assert.Contains(t, m.Payload.CompactLine, "Deploying", "the caption itself still arrives")
}

// TestMainSender_PlanUpdateIsInert covers the full-screen overlay. PlanName and
// each item's Label/Details/Output come straight from the agent's own
// update_plan meta tool — the same trust level as the reply text the user
// message route already sweeps — and the overlay is drawn over the whole frame,
// so an OSC in a label retitles the terminal window from inside it.
func TestMainSender_PlanUpdateIsInert(t *testing.T) {
	sink := &RecordingSink{}
	s := &localSender{sink: newInertSink(sink)}
	_, err := s.Send(context.Background(), sessInfo(),
		mustEnv(t, channelevents.KindPlanUpdate, channelevents.PlanUpdatePayload{
			PlanName: "Ship it" + oscTitle,
			Items: []channelevents.PlanItemRef{
				{ID: "i1", Label: "write tests" + cursorRepaint, Status: "done"},
				{ID: "i2", Label: "deploy", Status: "pending",
					Details: "needs approval" + cursorRepaint, Output: "ok" + oscTitle},
			},
			Diff: channelevents.PlanDiff{Added: []string{"i2" + cursorRepaint}},
		}))
	require.NoError(t, err, "Send")

	m, ok := sink.Events()[0].(MsgPlanUpdate)
	require.True(t, ok, "want MsgPlanUpdate, got %T", sink.Events()[0])

	assert.NotContains(t, m.Payload.PlanName, "\x1b",
		"the plan name is drawn as the overlay's header")
	for i, it := range m.Payload.Items {
		assert.NotContains(t, it.Label, "\x1b", "items[%d].label is drawn as a checklist row", i)
		assert.NotContains(t, it.Details, "\x1b", "items[%d].details", i)
		assert.NotContains(t, it.Output, "\x1b", "items[%d].output", i)
	}
	assert.NotContains(t, m.Payload.Diff.Added[0], "\x1b", "the diff is rendering-hint text too")
	assert.Equal(t, "write tests", m.Payload.Items[0].Label, "the label itself still arrives")
}

// TestMainSender_ToolProgressLabelsAreInert is not in the finding — the sweep
// found it. TailLine is documented as the "last meaningful console line" of a
// running tool, i.e. tool output, and Name is the tool's own name; both are
// rendered as a caption while the LLM is blocked and cannot narrate.
func TestMainSender_ToolProgressLabelsAreInert(t *testing.T) {
	sink := &RecordingSink{}
	s := &localSender{sink: newInertSink(sink)}
	_, err := s.Send(context.Background(), sessInfo(),
		mustEnv(t, channelevents.KindToolProgress, channelevents.ToolProgressPayload{
			CallID: "tc-1", Name: "git clone" + oscTitle,
			TailLine: "Receiving objects: 42%" + cursorRepaint,
		}))
	require.NoError(t, err, "Send")

	m, ok := sink.Events()[0].(MsgToolProgress)
	require.True(t, ok, "want MsgToolProgress, got %T", sink.Events()[0])
	assert.NotContains(t, m.Name, "\x1b")
	assert.NotContains(t, m.TailLine, "\x1b",
		"a tool's tail line is a caption slot, not the tool-output block: it keeps no colour and no cursor control")
}

// TestPermissionRequestSender_PayloadIsInert covers the join-request note.
// Preview is the joining USER's message text — cross-user by construction on
// any kind that can carry a join request. Nothing attacker-controlled reaches
// it on a single-user local session today; the door sweeps it because the door
// sweeps everything, not because this one is live.
func TestPermissionRequestSender_PayloadIsInert(t *testing.T) {
	t.Run("request: Preview cannot repaint the timeline", func(t *testing.T) {
		sink := &RecordingSink{}
		s := &permissionRequestSender{sink: newInertSink(sink)}
		_, err := s.Send(context.Background(), sessInfo(),
			mustEnv(t, channelevents.KindPermissionRequest, channelevents.PermissionRequestPayload{
				AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
				Preview:         "can I join?" + cursorRepaint,
				AgentClassName:  "demo-agent" + oscTitle,
				LinkedServices:  []string{"Tracker" + cursorRepaint},
			}))
		require.NoError(t, err, "Send")
		m := sink.Events()[0].(MsgPermissionRequest)
		assert.NotContains(t, m.Payload.Preview, "\x1b")
		assert.NotContains(t, m.Payload.AgentClassName, "\x1b")
		assert.NotContains(t, m.Payload.LinkedServices[0], "\x1b")
	})

	t.Run("applied: the decision Reason cannot repaint the timeline", func(t *testing.T) {
		sink := &RecordingSink{}
		s := &permissionRequestSender{sink: newInertSink(sink)}
		_, err := s.Send(context.Background(), sessInfo(),
			mustEnv(t, channelevents.KindPermissionDecisionApplied, channelevents.PermissionDecisionAppliedPayload{
				AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
				Decision:        "deny" + cursorRepaint, Reason: "timeout" + oscTitle,
			}))
		require.NoError(t, err, "Send")
		m := sink.Events()[0].(MsgPermissionDecisionApplied)
		assert.NotContains(t, m.Payload.Decision, "\x1b")
		assert.NotContains(t, m.Payload.Reason, "\x1b")
	})
}

// TestQueuedMessagesSender_InterruptReasonIsInert: Reason is the runner's
// human-readable explanation for refusing an interrupt, rendered as a timeline
// note beside the swept ones.
func TestQueuedMessagesSender_InterruptReasonIsInert(t *testing.T) {
	sink := &RecordingSink{}
	s := &queuedMessagesSender{sink: newInertSink(sink)}
	_, err := s.Send(context.Background(), sessInfo(),
		mustEnv(t, channelevents.KindInterruptApplied, channelevents.InterruptAppliedPayload{
			RequestID: "r-1", Outcome: "rejected",
			Reason: "no turn in flight" + cursorRepaint,
		}))
	require.NoError(t, err, "Send")

	m := sink.Events()[0].(MsgInterruptApplied)
	assert.NotContains(t, m.Reason, "\x1b")
	assert.Equal(t, "no turn in flight", m.Reason, "the explanation itself still arrives")
}

// TestSendErrorIsInert: the failure path renders a warning line built from an
// error string, and the error carries the decode failure's own text. Also not
// in the finding.
func TestSendErrorIsInert(t *testing.T) {
	sink := &RecordingSink{}
	s := &localSender{sink: newInertSink(sink)}
	// An envelope kind this sender does not handle: the default arm emits a
	// MsgSendError whose Err embeds the kind off the wire.
	_, err := s.Send(context.Background(), sessInfo(), channelevents.Envelope{
		Kind: channelevents.Kind("bogus" + cursorRepaint),
	})
	require.Error(t, err, "an unsupported kind must fail loudly")

	m, ok := sink.Events()[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", sink.Events()[0])
	assert.NotContains(t, string(m.Kind), "\x1b")
	assert.NotContains(t, m.Err, "\x1b")
}

// TestInertSink_SweepsAnEventShapeThisKindHasNeverEmitted is the point of the
// whole change, and the reason this is not a seventh enumeration.
//
// unknownRenderEvent is not a type this package emits, is not named anywhere in
// inert.go, and carries its strings behind every container the door has to walk
// to reach them. If the sweep were a list of known message types — the shape
// every previous pass had — this test could not pass. It passing is the claim
// that a message type added tomorrow, by someone who has never read inert.go,
// is swept with no edit to the seam at all.
func TestInertSink_SweepsAnEventShapeThisKindHasNeverEmitted(t *testing.T) {
	type deep struct{ Text string }
	type unknownRenderEvent struct {
		Lead      string
		Nested    deep
		Ptr       *deep
		List      []string
		Structs   []deep
		ByName    map[string]string
		Raw       []byte
		Untouched int
	}

	sink := &RecordingSink{}
	newInertSink(sink).Emit(unknownRenderEvent{
		Lead:      "lead" + cursorRepaint,
		Nested:    deep{Text: "nested" + cursorRepaint},
		Ptr:       &deep{Text: "behind a pointer" + oscTitle},
		List:      []string{"a" + cursorRepaint, "b"},
		Structs:   []deep{{Text: "in a slice" + cursorRepaint}},
		ByName:    map[string]string{"k" + oscTitle: "v" + cursorRepaint},
		Raw:       []byte("bytes" + cursorRepaint),
		Untouched: 7,
	})

	require.Len(t, sink.Events(), 1)
	got, ok := sink.Events()[0].(unknownRenderEvent)
	require.True(t, ok, "the door must hand the surface the same type it was given, got %T", sink.Events()[0])

	assert.NotContains(t, got.Lead, "\x1b", "a plain field")
	assert.NotContains(t, got.Nested.Text, "\x1b", "a nested struct")
	require.NotNil(t, got.Ptr)
	assert.NotContains(t, got.Ptr.Text, "\x1b", "behind a pointer")
	assert.NotContains(t, strings.Join(got.List, ""), "\x1b", "a slice of strings")
	assert.NotContains(t, got.Structs[0].Text, "\x1b", "a slice of structs")
	for k, v := range got.ByName {
		assert.NotContains(t, k, "\x1b", "a map key")
		assert.NotContains(t, v, "\x1b", "a map value")
	}
	assert.NotContains(t, string(got.Raw), "\x1b", "a []byte field")
	assert.Equal(t, 7, got.Untouched, "non-text fields pass through untouched")
}

// A shape-driven walk has to survive a shape that points back at itself, which
// is the cost of not enumerating types. The guard must TERMINATE (the naive
// walk takes the whole TUI process down with the stack) and must fail CLOSED at
// the limit — the deep value is dropped, never handed to the terminal unswept.
func TestInertSink_SelfReferentialEventTerminatesAndFailsClosed(t *testing.T) {
	type loop struct {
		Text string
		Next *loop
	}
	self := &loop{Text: "top" + cursorRepaint}
	self.Next = self // a cycle: every Next dereference finds another Next

	sink := &RecordingSink{}
	newInertSink(sink).Emit(*self)

	require.Len(t, sink.Events(), 1, "the walk must terminate rather than exhaust the stack")
	got := sink.Events()[0].(loop)
	assert.Equal(t, "top", got.Text, "the reachable text is still swept and still delivered")

	depth := 0
	for n := got.Next; n != nil; n = n.Next {
		assert.NotContains(t, n.Text, "\x1b", "no level may carry an unswept string")
		depth++
		require.Less(t, depth, 100, "the chain must be finite")
	}
	assert.Positive(t, depth, "the chain is walked, not discarded at the first hop")
}

// The door must not mutate what the caller still holds: a sender that keeps a
// reference to the payload it emitted (or a test that asserts on it) must see
// its own value, not a swept one.
func TestInertSink_DoesNotMutateTheCallersValue(t *testing.T) {
	sink := &RecordingSink{}
	items := []channelevents.PlanItemRef{{ID: "i1", Label: "deploy" + cursorRepaint}}
	msg := MsgPlanUpdate{Payload: channelevents.PlanUpdatePayload{PlanName: "p", Items: items}}

	newInertSink(sink).Emit(msg)

	assert.Contains(t, items[0].Label, "\x1b",
		"the sweep works on a copy; the caller's own slice is untouched")
	assert.NotContains(t, sink.Events()[0].(MsgPlanUpdate).Payload.Items[0].Label, "\x1b",
		"...and what reached the surface is swept")
}

// TestNewHost_WrapsTheSinkItWasGiven pins the door in place. Every real sender
// this kind hands out is built from Host.sink, so wrapping once here is what
// makes the coverage a property of the kind rather than a habit of each sender
// file. The field's TYPE is what stops a sender being built around a raw sink;
// this asserts the Host does not hand its senders one.
func TestNewHost_WrapsTheSinkItWasGiven(t *testing.T) {
	raw := &RecordingSink{}
	h := newTestHost(t, raw)
	require.NotNil(t, h.sink, "the host's sink")
	assert.Same(t, raw, h.sink.inner,
		"the host must wrap the caller's sink, not replace or double-wrap it")
}
