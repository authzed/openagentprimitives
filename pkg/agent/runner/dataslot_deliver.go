package runner

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// deliverNewBoundSlots places the content of any data slot bound since this
// session last looked, as a turn in its own right.
//
// # Why a turn and not just an in-memory block
//
// The transcript is the durable record, so a delivered datum has to be IN it:
// a child that is reaped and re-hydrated replays its turns and must still have
// the data it was working from. Appending only to the live message list would
// lose it on the first restart, silently, leaving a child that had the datum
// before the reap and does not after.
//
// # The already-delivered check
//
// A tag's id in a prior turn IS the delivery record — no side table, no new
// memory Kind, and it survives a restart because the transcript does. Keyed on
// the TAG rather than the slot so that a slot re-pointed at a different datum
// delivers again: the second datum is not the first, however the slot is
// named.
//
// # Called from BOTH resume paths, because a child takes one or the other by
// timing alone
//
// A child parked on request_input leaves by whichever happens first: the await
// TTL expires and it idle-exits (resuming later through Run start), or the
// parent answers inside the TTL and it resumes LIVE, in-loop, without ever
// restarting. Nothing about the delegation decides which — only how fast the
// parent replied.
//
// So this has to run at both. It did not: only Run start called it, and a
// child that got a fast reply continued with the parent's "sent it" message and
// no datum, having asked for that datum and been told it was on its way. The
// bundle covering the flow passed or failed on the same code depending on
// machine load, which is how it was found.
//
// prior is the transcript as of Run start, so it cannot see a delivery this
// same loop already made — deliveredSlotTags covers that half, and the two
// together make the call idempotent from either site.
//
// # awaitBind: the parent answered, so the datum is in flight
//
// The parent's REPLY and the parent's DATUM travel separately. send_input hands
// the tag to the operator, which grades it, may route it to a person, and only
// then writes the binding onto the request; reply_to_subagent wakes the child
// immediately. The wake routinely wins, and a child that looks once finds
// nothing bound and continues — having asked for the data and been told it was
// sent.
//
// That is a read-after-write visibility race, not a refusal, and it is the same
// one drainAwaitResume already bounds for a human's reply: look again, a few
// times, briefly. Callers pass awaitBind when the resume was triggered by a
// parent answer; every other resume looks exactly once, because there is
// nothing in flight to wait for.
//
// Bounded and then abandoned, never blocking: the design permits a parent to
// answer WITHOUT sending data ("the data may still not arrive"), and that case
// is indistinguishable from a slow bind. Waiting longer would turn a legitimate
// decline into a stall. The give-up is logged, and a later resume re-checks.
//
// Returns the blocks to append to the live request, already persisted.
func (l *Loop) deliverNewBoundSlots(ctx context.Context, prior []memory.Turn, awaitBind bool) []memory.ContentBlock {
	if l.ResolveBoundSlots == nil {
		return nil
	}
	slots, err := l.resolveBoundSlotsWaiting(ctx, prior, awaitBind)
	if err != nil {
		// Logged, not fatal, and NOT retried into a failure: the child is
		// running and may well be able to finish without the datum. Failing
		// the session here would turn "the data did not arrive" — a case the
		// design explicitly permits — into a dead delegation.
		slog.Default().Info("data slots: could not resolve bound inputs on resume; the child continues without them",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		return nil
	}
	var fresh []SlotDatum
	for _, s := range slots {
		if s.Content == "" || tagAlreadyDelivered(prior, s.TagID) || l.deliveredSlotTags[s.TagID] {
			continue
		}
		fresh = append(fresh, s)
	}
	if len(fresh) == 0 {
		return nil
	}

	// Marked BEFORE the append, so a storage failure below cannot cause a
	// second delivery of the same datum into the same live loop: the blocks go
	// to the model either way (see the append's comment), and delivering them
	// twice would read to the model as two distinct handovers of one fact.
	if l.deliveredSlotTags == nil {
		l.deliveredSlotTags = map[string]bool{}
	}
	for _, s := range fresh {
		l.deliveredSlotTags[s.TagID] = true
	}

	blocks := SlotContentBlocks(fresh)
	t := memory.Turn{
		Role:      "user",
		Content:   blocks,
		CreatedAt: time.Now().UTC(),
	}
	if err := l.Memory.Append(ctx, t); err != nil {
		// Appended-or-not decides whether this survives a restart, but NOT
		// whether the child sees it now — the blocks are returned either way,
		// because a datum the platform has already cleared for this child is
		// better in front of it than withheld over a storage hiccup.
		slog.Default().Info("data slots: delivering bound inputs to the transcript failed; the child sees them this turn but not after a restart",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "slots", len(fresh), "err", err.Error())
	}
	slog.Default().Info("data slots: delivered bound inputs on resume",
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "slots", len(fresh))
	return blocks
}

// tagAlreadyDelivered reports whether a tag's content is already in the
// transcript, by looking for the reference line SlotContentBlocks writes.
func tagAlreadyDelivered(prior []memory.Turn, tagID string) bool {
	if tagID == "" {
		return false
	}
	marker := slotRefMarker(tagID)
	for _, t := range prior {
		for _, b := range t.Content {
			if strings.Contains(b.Text, marker) {
				return true
			}
		}
	}
	return false
}

// slotBindRetries and slotBindRetryBackoff bound the wait for a binding the
// parent's answer says is coming. Deliberately the same shape and budget as
// awaitResumeDrainRetries — it is the same class of race, between the same two
// writers, and a second set of numbers to reason about buys nothing.
const (
	slotBindRetries      = 5
	slotBindRetryBackoff = 100 * time.Millisecond
)

// resolveBoundSlotsWaiting resolves this session's bound slots, re-reading a
// bounded number of times when awaitBind says one is in flight and the first
// read turned up nothing new. See deliverNewBoundSlots' awaitBind section.
//
// "Nothing new" rather than "nothing at all": a child resuming for the SECOND
// time already holds its first datum, so a non-empty result proves nothing
// about the datum it is waiting for now.
func (l *Loop) resolveBoundSlotsWaiting(
	ctx context.Context, prior []memory.Turn, awaitBind bool,
) ([]SlotDatum, error) {
	slots, err := l.ResolveBoundSlots(ctx)
	if err != nil || !awaitBind || l.hasUndelivered(prior, slots) {
		return slots, err
	}
	for attempt := 1; attempt <= slotBindRetries; attempt++ {
		select {
		case <-ctx.Done():
			return slots, nil
		case <-time.After(slotBindRetryBackoff):
		}
		// A re-read error is returned like any other: the caller's handler
		// already says why a resolve failure must not be silent.
		slots, err = l.ResolveBoundSlots(ctx)
		if err != nil || l.hasUndelivered(prior, slots) {
			return slots, err
		}
	}
	slog.Default().Info("data slots: the delegating agent answered but bound nothing within the wait; "+
		"continuing without it (this is also what a parent that chose not to send data looks like, "+
		"and a later resume re-checks)",
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
		"retries", slotBindRetries)
	return slots, nil
}

// hasUndelivered reports whether any resolved slot holds content this session
// has not already been given — the same test the delivery loop applies.
func (l *Loop) hasUndelivered(prior []memory.Turn, slots []SlotDatum) bool {
	for _, s := range slots {
		if s.Content == "" || tagAlreadyDelivered(prior, s.TagID) || l.deliveredSlotTags[s.TagID] {
			continue
		}
		return true
	}
	return false
}
