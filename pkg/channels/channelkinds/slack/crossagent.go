package slack

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Cross-agent participation in a human thread.
//
// The listener rejects every bot-authored message today, in one condition that
// folds THREE different cases together:
//
//	if ev.User == "" || ev.User == l.botUserID || ev.BotID != "" { return }
//
// Only the last may ever be relaxed, and separating them is the point of this
// file. Admitting our OWN posts is an immediate self-loop — the agent
// answering itself — and it would look exactly like the feature working right
// up until the thread ran away. In a single `||` chain, relaxing the wrong arm
// is a one-character mistake.

// botOrigin classifies who authored an inbound Slack event.
type botOrigin int

const (
	// originHuman: an ordinary person's message. Always admitted.
	originHuman botOrigin = iota

	// originSelf: OUR bot. NEVER admitted, in any configuration, because
	// answering our own post is a loop with no second party in it at all.
	originSelf

	// originOtherAgent: a different bot — the case cross-agent participation
	// exists for. Admitted only when enabled for the binding.
	originOtherAgent

	// originUnattributed: no user and no bot id. Never admitted: it is not
	// attributable to anyone, so neither mention-gating nor credit can be
	// charged to a party.
	originUnattributed
)

// classifyOrigin decides who sent an event, from the user and bot ids Slack
// supplies.
//
// SELF IS CHECKED FIRST and independently of BotID. Our own posts carry our
// bot id too, so an order that tested BotID first would classify us as another
// agent and admit the self-loop — which is the exact failure this split exists
// to make impossible to write by accident.
func classifyOrigin(userID, botID, selfBotUserID string) botOrigin {
	if selfBotUserID != "" && userID == selfBotUserID {
		return originSelf
	}
	if botID != "" {
		return originOtherAgent
	}
	if userID == "" {
		return originUnattributed
	}
	return originHuman
}

// admitsInbound reports whether an event of this origin may enter the
// pipeline at all.
//
// "Enter the pipeline" is not "wake the agent". An admitted cross-agent
// message still only APPENDS unless mention-gating and wake credit both allow
// a wake — see pipeline.SpendForAgentWake. Seeing is free; being driven is
// what is bounded.
func admitsInbound(o botOrigin, crossAgentEnabled bool) bool {
	switch o {
	case originHuman:
		return true
	case originOtherAgent:
		return crossAgentEnabled
	default:
		// originSelf and originUnattributed. Named as the default arm rather
		// than listed, so a future origin added to the enum is refused until
		// somebody decides otherwise.
		return false
	}
}

// crossAgentConfig returns the admission flag and wake budget resolved from
// the bound AgentClass, as of the last successful class read.
//
// Both are returned together, from one lock hold, because a caller that read
// them separately could admit a message under a class that had just been
// turned off and then charge it against the budget of the one that replaced
// it. They are written together for the same reason.
func (l *slackListener) crossAgentConfig() (enabled bool, wakeBudget int) {
	l.scopesMu.Lock()
	defer l.scopesMu.Unlock()
	return l.crossAgentEnabled, l.crossAgentWakeBudget
}

// setCrossAgent records what the bound class declares. Called only from the
// class-read path in activeFeatures, so the answer always describes a class
// that was actually read on this tick.
func (l *slackListener) setCrossAgent(class *spiceboxv1alpha1.AgentClass) {
	var enabled bool
	var budget int
	if class != nil {
		cfg := class.Spec.GetAuthz().GetCrossAgentThreads()
		enabled = cfg.Enabled
		budget = cfg.WakeBudget
	}
	l.scopesMu.Lock()
	defer l.scopesMu.Unlock()
	l.crossAgentEnabled = enabled
	l.crossAgentWakeBudget = budget
}
