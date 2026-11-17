package runner

import (
	"sort"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalactor"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// The turn ref names the inbound whose request the agent is working on: the
// InboundRef channelsd stamped on it, which its goal_actor attestation also
// carries. The goals service uses it only to SELECT a signed attestation, and
// refuses when anyone else has been attested since. Unlike advanceRequester's
// subject it never follows "whoever spoke last": a reply that lands while the
// agent works on someone else's request must not become that request's
// author.
//
// The rules, all applied on the loop goroutine:
//   - A turn OPENS at rest — the cold-start prompt, the drain that wakes a
//     resumed session, the drain after the resume park, and inbounds queued
//     behind agent_work_complete. The ref becomes the first human inbound's.
//   - A MID-TURN drain (an await resume, an interrupt) advances the ref only
//     when every human in it is the turn's opener, to the latest of theirs.
//     Anyone else in the drain leaves the ref alone, so the service keeps
//     refusing until the opener speaks again or the turn ends.
//   - Unknown is "" and the service refuses it. Nothing ever falls back to an
//     earlier turn's ref.

// TurnRef returns the current turn's originating-inbound ref, or "" when it
// is unknown. Safe from any goroutine.
func (l *Loop) TurnRef() string {
	if p := l.turnRef.Load(); p != nil {
		return *p
	}
	return ""
}

func (l *Loop) setTurn(opener identity.Subject, ref string) {
	l.turnOpener = opener
	l.turnRef.Store(&ref)
}

// startTurnOnNextDrain marks the next drainInbox as opening a new turn.
func (l *Loop) startTurnOnNextDrain() { l.turnStartPending = true }

// startInitialPromptTurn opens the cold-start turn, whose inbound is the
// session's opening prompt rather than an inbox item.
func (l *Loop) startInitialPromptTurn() {
	l.turnStartPending = false
	l.setTurn(l.authorForStarter(), goalactor.InitialPromptRef)
}

// noteDrainForTurn applies the turn rules to one drain's inbox items, in
// placement order. The pending-open mark is consumed even when nothing is
// held, so a later mid-turn drain can never be mistaken for a turn's start.
func (l *Loop) noteDrainForTurn(inbox []memory.Turn) {
	opens := l.turnStartPending
	l.turnStartPending = false
	if opens {
		if len(inbox) == 0 {
			return
		}
		// Authorless items (operator notices, thread catch-ups) name nobody;
		// a human item with no ref leaves the turn unattributable.
		l.setTurn("", "")
		for _, it := range inbox {
			if !it.Author.Empty() {
				l.setTurn(it.Author, it.InboundRef)
				return
			}
		}
		return
	}
	if l.turnOpener.Empty() {
		return
	}
	latest := ""
	for _, it := range inbox {
		if it.Author.Empty() {
			continue
		}
		if it.Author != l.turnOpener {
			return
		}
		if it.InboundRef != "" {
			latest = it.InboundRef
		}
	}
	if latest != "" {
		l.setTurn(l.turnOpener, latest)
	}
}

// transcriptAtRest reports whether a replayed transcript ended between turns:
// its last assistant turn either called agent_work_complete (successfully) or
// emitted no tool_use (the idle-yield recovery), and no human message was
// placed after it. Anything else — an await, a tool batch in flight, a
// refused turn awaiting retry, an unanswered human message — is mid-turn.
func transcriptAtRest(prior []memory.Turn) bool {
	last := -1
	for i, t := range prior {
		if t.Role == "assistant" && (last < 0 || t.Index > prior[last].Index) {
			last = i
		}
	}
	if last < 0 {
		return false
	}
	a := prior[last]
	for _, t := range prior {
		if t.Role == "user" && !t.Author.Empty() && t.Index > a.Index {
			return false
		}
	}
	return endsTurn(a, prior)
}

// endsTurn reports whether an assistant turn closed its turn.
func endsTurn(a memory.Turn, prior []memory.Turn) bool {
	if a.Refused {
		return false
	}
	var uses []*memory.ToolUseBlock
	for _, b := range a.Content {
		if b.Type == "tool_use" && b.ToolUse != nil {
			uses = append(uses, b.ToolUse)
		}
	}
	if len(uses) == 0 {
		return true
	}
	for _, u := range uses {
		if u.Name != "agent_work_complete" {
			continue
		}
		// A completion the gate refused came back as an error result and the
		// turn went on.
		for _, t := range prior {
			for _, b := range t.Content {
				if b.Type == "tool_result" && b.ToolResult != nil && b.ToolResult.ToolUseID == u.ID && b.ToolResult.IsError {
					return false
				}
			}
		}
		return true
	}
	return false
}

// resumeTurnState recovers the in-flight turn of a session restarted
// mid-turn: its opener is the first human turn in the turn's opening batch
// (the user turns after the last turn-closing assistant turn and before the
// next assistant turn), and its ref advances through that opener's later turns
// until anyone else speaks. Turn 0 is the opening prompt. With no
// recoverable opener or ref the turn stays unattributable ("").
func (l *Loop) resumeTurnState(prior []memory.Turn) {
	l.turnStartPending = false
	l.setTurn("", "")
	sorted := make([]memory.Turn, 0, len(prior))
	for _, t := range prior {
		if t.Role == "user" || t.Role == "assistant" {
			sorted = append(sorted, t)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })
	start := 0
	for i, t := range sorted {
		if t.Role == "assistant" && endsTurn(t, prior) {
			start = i + 1
		}
	}
	// The opener comes only from the OPENING BATCH: the user turns before the
	// turn's first assistant turn, as the live turn-opening drain sees them.
	// If that batch has no human, the live turn had no opener and later
	// replies never gained one, so a later replier must not become it here.
	var opener identity.Subject
	ref := ""
	inOpening := true
	for _, t := range sorted[start:] {
		if t.Role == "assistant" {
			if opener.Empty() {
				return
			}
			inOpening = false
			continue
		}
		if t.Author.Empty() {
			continue
		}
		r := t.InboundRef
		if t.Index == 0 {
			r = goalactor.InitialPromptRef
		}
		if opener.Empty() {
			if !inOpening {
				return
			}
			opener, ref = t.Author, r
			continue
		}
		if inOpening {
			// Later humans in the opening batch never moved the live ref.
			continue
		}
		if t.Author != opener {
			break
		}
		if r != "" {
			ref = r
		}
	}
	if opener.Empty() {
		return
	}
	l.setTurn(opener, ref)
}
