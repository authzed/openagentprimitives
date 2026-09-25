//go:build e2e

// leads_console_action_test.go is spec E2E 14 and 16, in one file: they
// share every fixture and differ only in when the live socket opens. 14 is
// "click a side-effecting action -> an approver decides -> the action
// resolves to succeeded and the UI re-renders"; 16 is "reload mid-approval
// -> the pending action is still awaiting_approval and still resolves",
// expressed here as opening the live socket AFTER the action was fired
// (step 3 below) rather than before — the same observable a browser reload
// would produce. Chrome auto-reveal (the design spec's one non-negotiable)
// is a browser-side property with its own frontend coverage from Plan 5 and
// is deliberately NOT asserted here.
package agentui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uidemo/leadflow"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// terminalUIActionStates mirrors uiaction.IsTerminal's own switch as a
// string set — leads_console_read_test.go's sibling assertions use
// assert.NotContains against a literal for the same reason: pinning one
// exact parked-state string would encode a race (submitted vs
// awaiting_approval is a timing accident, not a contract) as if it were a
// requirement.
var terminalUIActionStates = []string{
	string(uiaction.StateSucceeded), string(uiaction.StateFailed), string(uiaction.StateDenied),
	string(uiaction.StateExpired), string(uiaction.StateRateLimited),
}

// liveActionEntryMirror mirrors pkg/web/webui/agentui's unexported
// liveActionEntry (live.go) — the shape carried inside a "snapshot" frame's
// Actions array and an "event" frame's Action object. Duplicated rather than
// imported for the same reason every other *Mirror type in this package is:
// the server type is unexported in a package this test must not depend on
// for its import graph.
type liveActionEntryMirror struct {
	RequestID                 string `json:"requestId"`
	Action                    string `json:"action"`
	State                     string `json:"state"`
	Message                   string `json:"message,omitempty"`
	ApprovalAddressedToViewer bool   `json:"approvalAddressedToViewer,omitempty"`
}

// decodeSnapshotActions decodes a "snapshot"-typed LiveFrame's Actions array.
func decodeSnapshotActions(t *testing.T, frame e2e.LiveFrame) []liveActionEntryMirror {
	t.Helper()
	require.Equal(t, "snapshot", frame.Type, "expected the action snapshot frame")
	var entries []liveActionEntryMirror
	if len(frame.Actions) > 0 {
		require.NoError(t, json.Unmarshal(frame.Actions, &entries), "decode snapshot actions")
	}
	return entries
}

// findActionEntry returns the entry naming requestID, if present.
func findActionEntry(entries []liveActionEntryMirror, requestID string) (liveActionEntryMirror, bool) {
	for _, e := range entries {
		if e.RequestID == requestID {
			return e, true
		}
	}
	return liveActionEntryMirror{}, false
}

// awaitActionEvent drains "event"-typed frames from ch (ignoring "view" and
// any other frame type) until one names requestID with a State in want, or
// deadline elapses. Returns the matching entry. Used AFTER the snapshot has
// already been read once (decodeSnapshotActions is the caller's first read),
// so this only ever needs to see subsequent pushes.
func awaitActionEvent(t *testing.T, ch <-chan e2e.LiveFrame, requestID string, deadline time.Time, want ...string) liveActionEntryMirror {
	t.Helper()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("awaitActionEvent: timed out waiting for requestID=%s state in %v", requestID, want)
		}
		select {
		case frame, ok := <-ch:
			if !ok {
				t.Fatalf("awaitActionEvent: live socket closed before requestID=%s reached state in %v", requestID, want)
			}
			if frame.Type != "event" || len(frame.Action) == 0 {
				continue
			}
			var e liveActionEntryMirror
			if err := json.Unmarshal(frame.Action, &e); err != nil {
				continue
			}
			if e.RequestID != requestID || !slices.Contains(want, e.State) {
				continue
			}
			return e
		case <-time.After(remaining):
			t.Fatalf("awaitActionEvent: timed out waiting for requestID=%s state in %v", requestID, want)
		}
	}
}

// advanceLeadStageCalls filters crmServer.Calls() to just the
// advance_lead_stage invocations, so a caller can assert an exact count —
// the diagnostic the task brief calls out by name: a call-log check that
// only inspects field values (rather than counting matches) can pass for
// the wrong reason when a mutant fires the call it should have refused but
// with unrelated args. Counting first, then inspecting fields on what was
// found, catches both.
func advanceLeadStageCalls(calls []leadflow.Call) []leadflow.Call {
	var out []leadflow.Call
	for _, c := range calls {
		if c.Tool == leadflow.ToolAdvanceLeadStage {
			out = append(out, c)
		}
	}
	return out
}

func findLeadByValue(leads []leadflow.Lead, value string) (leadflow.Lead, bool) {
	for _, l := range leads {
		if l.Value == value {
			return l, true
		}
	}
	return leadflow.Lead{}, false
}

// TestLeadsConsole_ActionApprovalRoundTrip drives the write half end to end:
// a click -> the actions endpoint -> the runner's approval branch (a
// side-effecting action never auto-runs — see handleAppToolCallReq's
// autoRun predicate, gated on StateImpact alone) -> the ui_action
// lifecycle record -> the outcome reaching the control the viewer is
// looking at.
func TestLeadsConsole_ActionApprovalRoundTrip(t *testing.T) {
	crmServer, crm := newLeadflowServer(t)
	h := e2e.Start(t, e2e.Options{})
	applyLeadsConsoleFixture(t, h, crm.URL)
	stampAgentUIValidity(t, h, "default", "pipeline-desk-ui", pipelineDeskGrantedTools)
	h.WaitForAgentClassValid("pipeline-desk", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)
	// Guardian's async schema composition (the slot_grant_owner relation
	// 03-agentclass.yaml's authz.slots injects onto dealbook_lead's "owner"
	// permission — see 01-mcpserver.yaml) is its own reconcile loop,
	// independent of AgentClass Valid=True and the SpiceDBBootstrap CR above
	// (which only ever seeds the unrelated "viewer" wildcard tuple). Without
	// this wait, advance_lead_stage's approval check can race a schema that
	// has not composed the slot yet — see
	// test/e2e/bronzethread/driver_test.go's waitForComposedSlotSchema,
	// which this mirrors for the same reason.
	h.WaitForComposedSchema(30*time.Second, []e2e.SchemaRel{
		{Definition: "dealbook_lead", Relation: authz.SlotGrantRelationName("advance")},
	})

	const viewerEmail = "viewer@demo.invalid"

	// Cold-start turn: same single-shot-terminal shape as
	// leads_console_read_test.go — see that file's identical rule for why
	// it must be Repeating() (a possible spec.Prompt replay) and why
	// agent_work_complete needs no respond_to_user round-trip.
	h.LLM.On(func(llm.Request) bool { return true }).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "opened the pipeline desk"})).
		Repeating()

	h.SendUserMessage("open the pipeline desk", e2e.AsUser(viewerEmail))
	e2e.WaitForSessionIdle(t, h)

	sess := e2e.FindOnlyAgentSession(t, h, h.Namespace())
	ns := h.Namespace()

	viewer := e2e.CanonicalForFakeEmail(viewerEmail)
	b := h.AgentUIBrowserFor("user:" + viewer.String())
	ctx := context.Background()

	// dealbook_lead's "owner" relation (01-mcpserver.yaml) needs a CONCRETE
	// subject tuple, not a wildcard — see 04-spicedbbootstrap.yaml's doc
	// comment for why a wildcard tuple would satisfy the composed
	// slot_grant_owner->interact leg for ANY subject but can never satisfy
	// the exact-match membership check ApprovalAddressedToViewer depends on.
	// Written here, at test runtime, because only the test knows its own
	// viewer's canonical subject ahead of time — same pattern
	// browserarm_test.go's self-test uses for agentsession#owner. Both
	// leads this test targets (the approve and deny paths, below) need
	// their own tuple naming the SAME viewer: this is a single-viewer
	// scenario where the clicking viewer is also the resource owner who
	// must decide, which is exactly the shape that makes
	// ApprovalAddressedToViewer meaningful to assert — and, since
	// advance_lead_stage is stateImpact: external (01-mcpserver.yaml), this
	// direct tuple deliberately does NOT let the check bypass approval; it
	// only proves the OWNER, not a stranger, is who gets asked.
	e2e.WriteRel(t, h, "dealbook_lead", "lead-aurorabyte", "owner", "user", viewer.String(), "")
	e2e.WriteRel(t, h, "dealbook_lead", "lead-brightfern", "owner", "user", viewer.String(), "")

	propsRaw, err := b.Page(ctx, ns, sess.Name)
	require.NoError(t, err, "the doors ladder must resolve for a live session the viewer may interact with")
	_, decl := decodePageProps(t, propsRaw)
	defaultParams := paramsFrom(uicomponents.ParamStates(decl))
	require.Len(t, defaultParams, 4, "window.from, window.to, stage, lead")

	deadline := time.Now().Add(30 * time.Second)

	// --- Approve path -------------------------------------------------

	const approveLeadID = "lead-aurorabyte" // seed.go: starts at stage "new"
	const approveNote = "qualifying after the demo call"

	params := make(map[string]string, len(defaultParams))
	for k, v := range defaultParams {
		params[k] = v
	}
	params["lead"] = approveLeadID

	// 1. Fire it. The synchronous answer is a PARKED state, never a
	// verdict — a side-effecting action always takes the approval branch
	// (7b in handleAppToolCallReq's doc comment), so pinning one exact
	// string here would encode a submitted-vs-awaiting_approval race as a
	// requirement.
	resp, status, err := b.Invoke(ctx, ns, sess.Name, "advance_stage", params, map[string]string{"note": approveNote})
	require.NoError(t, err, "Invoke transport must succeed")
	require.Equal(t, http.StatusOK, status, "a parked action is 200, not an HTTP error")
	require.NotEmpty(t, resp.RequestID, "the synchronous response must carry a correlation id")
	assert.NotContains(t, terminalUIActionStates, resp.State,
		"the synchronous answer for a side-effecting action must be PARKED, never a verdict")

	// 2. Nothing has happened upstream yet — this is what distinguishes
	// "parked for approval" from "ran and reported late"; no assertion on
	// state alone can.
	require.Empty(t, advanceLeadStageCalls(crmServer.Calls()),
		"advance_lead_stage must not have been called before an approval decision")

	// 3. Open a live socket AFTER the action was fired — spec E2E 16's
	// reload, expressed exactly as a browser reload expresses it. The
	// record was written to memory SYNCHRONOUSLY before Invoke's HTTP
	// response returned (uiActionRecorder.transition writes memory then
	// publishes, and the StateSubmitted transition runs on the same
	// goroutine before the detached approval exec is even spawned — see
	// apptoolcall.go), so the very FIRST frame this socket delivers (the
	// snapshot, always sent before any event — live.go's runActionMirror)
	// must already carry this request in a non-terminal state. An empty
	// snapshot here means the record is only in NATS and a reload loses
	// it.
	frames, closeLive, err := b.Live(ctx, ns, sess.Name)
	require.NoError(t, err, "dial the live socket")
	t.Cleanup(closeLive)

	firstFrame, ok := <-frames
	require.True(t, ok, "the live socket must deliver at least one frame")
	snapEntries := decodeSnapshotActions(t, firstFrame)
	snapEntry, found := findActionEntry(snapEntries, resp.RequestID)
	require.True(t, found, "the snapshot must already carry this action's record — an empty snapshot here means the record is only in NATS and a reload loses it")
	assert.NotContains(t, terminalUIActionStates, snapEntry.State, "the snapshot's entry must be non-terminal (still pending)")

	// 4. Approve, via the harness's real approval surface.
	approval := h.ExpectApprovalPrompt(
		e2e.ForTool("deals_advance_lead_stage"),
		e2e.ForResource("dealbook_lead:"+approveLeadID),
	)
	assert.Equal(t, "dealbook_lead:"+approveLeadID+"#owner", approval.Prompt().Approver,
		"approval must route to the resource's owner-set, not the session approve-set")

	// The awaiting_approval transition (which stamps
	// ApprovalAddressedToViewer) happens strictly BEFORE the interaction
	// reaches the fake channel driver that ExpectApprovalPrompt just
	// observed (host_approval.go's buildToolCallPending calls the
	// approvalEvent callback before returning the pendingApproval its
	// caller then publishes) — so it may have arrived as this socket's
	// FIRST snapshot entry already, or as an "event" frame since. Either
	// way, this is what Plan 5's C2 regression dropped: a viewer who must
	// approve their own action was never told.
	awaitingEntry := snapEntry
	if awaitingEntry.State != string(uiaction.StateAwaitingApproval) {
		awaitingEntry = awaitActionEvent(t, frames, resp.RequestID, deadline, string(uiaction.StateAwaitingApproval))
	}
	assert.True(t, awaitingEntry.ApprovalAddressedToViewer,
		"the clicking viewer is also the wildcarded resource owner — approvalAddressedToViewer must be true")

	approval.Approve(e2e.AsUser(viewerEmail))

	succeededEntry := awaitActionEvent(t, frames, resp.RequestID, deadline, string(uiaction.StateSucceeded))
	assert.Equal(t, string(uiaction.StateSucceeded), succeededEntry.State)

	// The CRM's OWN call log, not the response: the half that cannot be
	// faked by a stub that reports success without actually mutating.
	approveCalls := advanceLeadStageCalls(crmServer.Calls())
	require.Len(t, approveCalls, 1, "advance_lead_stage must have been called exactly once")
	assert.Equal(t, approveLeadID, approveCalls[0].Args["leadId"], "the call must carry the chosen leadId")
	assert.Equal(t, approveNote, approveCalls[0].Args["note"], "the call must carry the submitted note")

	advancedLead, foundLead := findLeadByValue(crmServer.Book().Filter("", "", ""), approveLeadID)
	require.True(t, foundLead, "the approved lead must still exist in the book")
	assert.Equal(t, "qualified", advancedLead.Stage, "the book must show the lead advanced by exactly one stage")

	// 5. The outcome reaches the control the viewer is looking at:
	// re-resolve bindings and assert the action:-source binding on the
	// ap:alert is now non-empty human copy, having been the empty string
	// on first paint (leads_console_read_test.go asserted that baseline).
	got, bstatus, err := b.Bindings(ctx, ns, sess.Name, defaultParams)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, bstatus)
	// advance is not agentWritable and compiles to no hook (region ""); its
	// ap:alert sits at root child 4 (advance), 0 (the inner stack), 2 (the
	// alert itself).
	var alertBody string
	require.NoError(t, json.Unmarshal(got["/4.0.2#body"].Value, &alertBody), "decode /4.0.2#body")
	assert.Equal(t, uiaction.DisplayCopy(uiaction.StateSucceeded, false), alertBody,
		"the oap:alert's action: binding must reflect the settled outcome")

	// --- Deny path, same file, same session -----------------------------

	const denyLeadID = "lead-brightfern" // seed.go: starts at stage "new", distinct from the approve path
	const denyNote = "declining for now"

	params["lead"] = denyLeadID
	resp2, status2, err := b.Invoke(ctx, ns, sess.Name, "advance_stage", params, map[string]string{"note": denyNote})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status2)
	require.NotEmpty(t, resp2.RequestID)
	assert.NotContains(t, terminalUIActionStates, resp2.State)

	denyApproval := h.ExpectApprovalPrompt(
		e2e.ForTool("deals_advance_lead_stage"),
		e2e.ForResource("dealbook_lead:"+denyLeadID),
	)
	denyApproval.Deny(e2e.AsUser(viewerEmail))

	deniedEntry := awaitActionEvent(t, frames, resp2.RequestID, deadline, string(uiaction.StateDenied))
	assert.Equal(t, string(uiaction.StateDenied), deniedEntry.State)

	// 6. A denial that still mutated would be the worst possible defect
	// here — assert both the call log (count first, so a wrongly-fired
	// call is caught even if its args happen to differ) and the book.
	assert.Empty(t, findLeadStageAdvanceForLead(crmServer.Calls(), denyLeadID),
		"advance_lead_stage must NOT have been called for the denied lead")
	deniedLead, foundDenied := findLeadByValue(crmServer.Book().Filter("", "", ""), denyLeadID)
	require.True(t, foundDenied)
	assert.Equal(t, "new", deniedLead.Stage, "the book must be unchanged after a denial")
}

// findLeadStageAdvanceForLead filters advance_lead_stage calls to just the
// ones naming leadID — used by the deny-path assertion so a call fired for
// a DIFFERENT lead (e.g. a leftover from the approve path above) can never
// be mistaken for one that should not have happened.
func findLeadStageAdvanceForLead(calls []leadflow.Call, leadID string) []leadflow.Call {
	var out []leadflow.Call
	for _, c := range advanceLeadStageCalls(calls) {
		if id, _ := c.Args["leadId"].(string); id == leadID {
			out = append(out, c)
		}
	}
	return out
}
