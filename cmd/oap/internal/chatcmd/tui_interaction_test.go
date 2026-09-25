package chatcmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	local "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
)

// decisionInteraction builds a generic interaction payload with two
// decision actions (approve/deny) — the tool_approval shape once Task 10 flips
// it onto the generic path. Category-generic: the modal binds keys off these
// actions, not off the category string.
func decisionInteraction() channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
		Category:        "tool_approval",
		RequestRef:      "r-1",
		Lead:            "Approve running git_push?",
		Fields:          []channelevents.InteractionField{{Label: "Tool", Value: "git_push"}},
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Style: channelevents.ActionStyleDanger, Kind: channelevents.ActionKindDecision},
		},
	}
}

// TestChatModel_InteractionModal_OpensAndDecides verifies a MsgInteractionRequest
// carrying decision actions opens the blocking modal and that a/d invoke
// m.decideInteraction(requestRef, category, "approve"|"deny") and dismiss it —
// the D1-regression fix that gives the local TUI a decision-input path on the
// generic interaction envelope (mirroring the typed approval modal).
func TestChatModel_InteractionModal_OpensAndDecides(t *testing.T) {
	// Opening the modal renders the Lead + the derived keybindings.
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, local.MsgInteractionRequest{Payload: decisionInteraction()})
	modal := m.View()
	assert.Contains(t, modal, "Decision required")
	assert.Contains(t, modal, "Approve running git_push?")
	assert.Contains(t, modal, "[a] Approve", "decision action bound to its ID's first rune")
	assert.Contains(t, modal, "[d] Deny")

	// Approve calls decideInteraction(requestRef, category, "approve") and
	// dismisses the modal.
	var gotRID, gotCat, gotAction string
	m2 := sized(newChatModel("demo-agent", "s1", true))
	m2.decideInteraction = func(rid, cat, action string) { gotRID, gotCat, gotAction = rid, cat, action }
	m2 = applyMsg(t, m2, local.MsgInteractionRequest{Payload: decisionInteraction()})
	m2 = applyMsg(t, m2, keyMsg("a"))
	assert.Equal(t, "r-1", gotRID)
	assert.Equal(t, "tool_approval", gotCat)
	assert.Equal(t, "approve", gotAction)
	assert.NotContains(t, m2.View(), "Decision required", "modal dismissed after decision")

	// Deny path publishes the deny action ID.
	m3 := sized(newChatModel("demo-agent", "s1", true))
	m3.decideInteraction = func(rid, cat, action string) { gotRID, gotCat, gotAction = rid, cat, action }
	m3 = applyMsg(t, m3, local.MsgInteractionRequest{Payload: decisionInteraction()})
	m3 = applyMsg(t, m3, keyMsg("d"))
	assert.Equal(t, "deny", gotAction)
}

// TestChatModel_InteractionModal_BlocksOtherInput verifies the decision modal
// has exclusive focus: a non-action key does not reach the input box and the
// modal stays up.
func TestChatModel_InteractionModal_BlocksOtherInput(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, local.MsgInteractionRequest{Payload: decisionInteraction()})
	m = applyMsg(t, m, keyMsg("z"))
	assert.Empty(t, m.input.Value(), "input box is frozen while the decision modal blocks")
	assert.Contains(t, m.View(), "Decision required", "modal still up after a non-action key")
}

// TestChatModel_InteractionRequest_LinkOnly_RendersNoteNotModal verifies the
// read-only path: an interaction with only link actions (no decision) opens no
// modal — its pre-rendered markdown floor lands in the timeline instead.
func TestChatModel_InteractionRequest_LinkOnly_RendersNoteNotModal(t *testing.T) {
	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
		Category:        "credential_link",
		RequestRef:      "cred-1",
		Lead:            "Link your account",
		Actions: []channelevents.InteractionAction{
			{ID: "open", Label: "Authenticate", Kind: channelevents.ActionKindLink, URL: "https://example.com/login"},
		},
	}
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, local.MsgInteractionRequest{Payload: pl, Text: "Link your account — https://example.com/login"})
	view := m.View()
	assert.NotContains(t, view, "Decision required", "a link-only interaction opens no decision modal")
	assert.Contains(t, view, "Link your account", "the read-only markdown floor is rendered to the timeline")
}

// TestChatModel_InteractionApplied_NeverSilent verifies a resolved
// interaction ALWAYS lands a visible timeline note.
//
// This TUI is the only surface a single-user session has, so an outcome it
// drops is an outcome nobody ever sees. It gated the note on OutcomeText
// being non-empty — and the two tool-approval decision handlers resolve with
// no OutcomeText at all, so approving a tool call dismissed the modal and
// recorded nothing: the same silent-resolution class as the rejected path
// this file already guards below.
func TestChatModel_InteractionApplied_NeverSilent(t *testing.T) {
	cases := []struct {
		name string
		msg  local.MsgInteractionApplied
		want string
	}{
		{
			name: "approved tool call: no OutcomeText, still records the outcome",
			msg:  local.MsgInteractionApplied{Category: "tool_approval", Outcome: channelevents.OutcomeApproved},
			want: "Approved",
		},
		{
			name: "denied tool call: reads as denied, never as the wire enum",
			msg:  local.MsgInteractionApplied{Category: "tool_approval", Outcome: channelevents.OutcomeDenied},
			want: "Denied",
		},
		{
			name: "expired: nobody decided, and the wait is over either way",
			msg:  local.MsgInteractionApplied{Category: "tool_approval", Outcome: channelevents.OutcomeExpired},
			want: "Expired",
		},
		{
			name: "identity_choice: the friendly label, not the raw wire action id",
			msg:  local.MsgInteractionApplied{Category: "identity_choice", Outcome: channelevents.OutcomeApproved, OutcomeText: "agent"},
			want: "Running as the agent",
		},
		{
			name: "credential_link: the category's own OutcomeText passes through",
			msg:  local.MsgInteractionApplied{Category: "credential_link", Outcome: channelevents.OutcomeResolved, OutcomeText: "GitHub"},
			want: "GitHub",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(newChatModel("demo-agent", "s1", true))
			m = applyMsg(t, m, local.MsgInteractionRequest{Payload: decisionInteraction()})
			m = applyMsg(t, m, tc.msg)

			assert.Contains(t, m.View(), tc.want)
			assert.NotContains(t, m.View(), "Decision required",
				"a resolved interaction dismisses the modal")
		})
	}
}

// TestChatModel_InteractionRejected_NeverSilent verifies every
// local.MsgInteractionRejected class renders a visible timeline note — the
// decision pipe never swallows a rejected click (no-silent-errors,
// AGENTS.md), and this TUI is the only surface a single-user session has, so
// a dropped rejection (especially handler_error, e.g. a tool_approval
// grant-write failure) would be a silent hang with no explanation.
func TestChatModel_InteractionRejected_NeverSilent(t *testing.T) {
	cases := []struct {
		name string
		msg  local.MsgInteractionRejected
		want string
	}{
		{
			name: "not_authorized: shows the reason",
			msg:  local.MsgInteractionRejected{Class: "not_authorized", Reason: "you are not authorized to decide this request"},
			want: "you are not authorized to decide this request.",
		},
		{
			name: "handler_error: shows the underlying failure — the grant-write case",
			msg:  local.MsgInteractionRejected{Class: "handler_error", Reason: "grant write failed: connection refused"},
			want: "grant write failed: connection refused.",
		},
		{
			name: "category_mismatch: shows the reason",
			msg:  local.MsgInteractionRejected{Class: "category_mismatch", Reason: "this action no longer matches the pending request"},
			want: "this action no longer matches the pending request.",
		},
		{
			name: "already_resolved: names the outcome, not a Slack-only mention",
			msg:  local.MsgInteractionRejected{Class: "already_resolved", Reason: "already resolved", OriginalOutcome: "approved"},
			want: "already resolved (approved).",
		},
		{
			name: "empty reason: falls back to a generic note rather than a blank line",
			msg:  local.MsgInteractionRejected{Class: "not_authorized"},
			want: "your click could not be applied.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(newChatModel("demo-agent", "s1", true))
			m = applyMsg(t, m, tc.msg)
			assert.Contains(t, m.View(), tc.want)
		})
	}
}

// Cross-surface parity for the plan-gate card. Its What is multi-line by
// construction — one line per permission in the phase's ceiling, then a line per
// resource — and the resource lines are what the approver is deciding on.
//
// Worth pinning per surface rather than once, because each folds whitespace
// differently and the failure is silent: webchat collapsed exactly this into a
// single unreadable line while Slack and the TUI did not, so the same approval
// read differently depending on where it was answered.
func TestRenderInteractionModal_keepsAPlanCardsResourceLines(t *testing.T) {
	what := "perm:fetch:git_repo\nperm:push:git_repo\n\nAlso asks to reach:\n" +
		"  git_repo — https://github.com/acme/app\n" +
		"Every resource above is named, so this approval covers them."

	p := decisionInteraction()
	p.Category = "plan_phase"
	p.Lead = `The agent is asking to run "Recon"`
	p.Fields = []channelevents.InteractionField{
		{Label: "What", Value: what},
		{Label: "Why (the agent's words)", Value: "the ticket names this repo"},
	}

	out := renderInteractionModal(p, local.SessionRef{Namespace: "default", Name: "demo-session"}, "demo-session", 100, 40, aptest.ColorTheme())

	assert.Contains(t, out, "https://github.com/acme/app",
		"the resource is the fact the approval is about; a surface that drops it asks for consent to something unseen")
	assert.Contains(t, out, "Every resource above is named",
		"the coverage line tells the approver whether saying yes finishes the request")
	assert.Contains(t, out, "Why (the agent's words)",
		"the agent's text stays attributed on every surface")
	// Asserted as "on different rows" rather than as an exact adjacency: the
	// modal pads every line to the box width, so the newline between them is
	// not literal. The property that matters is that they did not run together.
	var headerRow, resourceRow = -1, -1
	for i, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Also asks to reach:") {
			headerRow = i
		}
		if strings.Contains(line, "https://github.com/acme/app") {
			resourceRow = i
		}
	}
	assert.NotEqual(t, -1, headerRow, "the slot section header must render")
	assert.NotEqual(t, -1, resourceRow, "the resource must render")
	assert.NotEqual(t, headerRow, resourceRow,
		"the ceiling and the resources must not collapse onto one row — that is the webchat bug")
}
