package bronzethread

import "fmt"

// Goals is the goal-execution half of a bundle.
//
// A goal scenario does not end with its conversation: the conversation asks
// for future runs, a person decides a card, and the operator starts sessions
// nobody typed into. Three things a conversational bundle never needed follow
// from that, and they live together here so a bundle that uses none of them
// carries nothing.
type Goals struct {
	// Execution starts the durable goals runtime (consent, scheduling, the
	// dispatcher and run delivery). Without it goal tools still manage goals,
	// but nothing ever runs — which is what management-only bundles assert.
	Execution bool `json:"execution,omitempty"`

	// Conversation is the kind-specific External the conversation's inbounds
	// carry. The fake kind reads its group from External["group"], so this is
	// how a bundle puts its thread inside a group that can own goals.
	Conversation map[string]string `json:"conversation,omitempty"`

	// Audience is who reads the conversation, by email, as the fake kind's
	// AudienceResolver will report it. The fake kind has no membership source
	// of its own to enumerate, so its audience is fixture state; a group write
	// whose sources the audience cannot read is refused against it.
	Audience []string `json:"audience,omitempty"`

	// Decisions are explicit clicks on published cards, made in order by a
	// background clicker that starts before the first turn (a card can hold a
	// turn open). Unlike AutoApprove, each names who clicked and what the
	// click must lead to, so a refused click is a claim, not a hang. The run's
	// assertions wait for every decision to be made.
	Decisions []Decision `json:"decisions,omitempty"`
}

// Decision is one person's click on one published card.
type Decision struct {
	// Category is the card's interaction category.
	Category string `json:"category"`
	// Nth picks the card among those of this category, zero-based in publish
	// order. The driver waits for it to be published.
	Nth int `json:"nth,omitempty"`
	// As is who clicks.
	As string `json:"as"`
	// Action is "approve" or "deny".
	Action string `json:"action"`
	// Want is what the click must lead to: "applied" (the card was decided)
	// or "rejected" (the pipe refused the clicker).
	Want string `json:"want"`
}

// GoalRunAssertion is a claim about one goal occurrence's durable record.
type GoalRunAssertion struct {
	// Proposal is the run's reported status (reported_success, blocked, ...).
	Proposal string `json:"proposal,omitempty"`
	// SummaryContains must appear in the run's reported summary.
	SummaryContains string `json:"summaryContains,omitempty"`
	// Reply is the reply's delivery state (accepted, unconfirmed, ...).
	Reply string `json:"reply,omitempty"`
	// ApprovalPrompts is the exact number of cards published in this run's
	// own session. Zero is the claim a covered run exists to make.
	ApprovalPrompts *int `json:"approvalPrompts,omitempty"`
}

func (b Bundle) validateGoals() error {
	if b.Goals == nil {
		if len(b.Assert.GoalRuns) > 0 {
			return fmt.Errorf("assert.goalRuns needs goals.execution: without the runtime nothing runs")
		}
		return nil
	}
	if len(b.Assert.GoalRuns) > 0 && !b.Goals.Execution {
		return fmt.Errorf("assert.goalRuns needs goals.execution: without the runtime nothing runs")
	}
	for i, d := range b.Goals.Decisions {
		switch {
		case d.Category == "":
			return fmt.Errorf("goals.decisions[%d] has no category", i)
		case d.As == "":
			return fmt.Errorf("goals.decisions[%d] has no clicker (as)", i)
		case d.Action != "approve" && d.Action != "deny":
			return fmt.Errorf("goals.decisions[%d] action %q is not approve or deny", i, d.Action)
		case d.Want != "applied" && d.Want != "rejected":
			return fmt.Errorf("goals.decisions[%d] want %q is not applied or rejected", i, d.Want)
		case d.Nth < 0:
			return fmt.Errorf("goals.decisions[%d] nth is negative", i)
		}
	}
	return nil
}
