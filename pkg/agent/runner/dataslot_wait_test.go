package runner

// The gap between the parent's ANSWER and the parent's DATUM.
//
// reply_to_subagent wakes the child immediately; send_input's binding only
// lands after the operator grades it and, sometimes, after a person approves.
// The wake routinely wins that race, and a child that looked exactly once
// carried on without the thing it had asked for and been promised — the
// e2e bundle covering the flow passed or failed on machine load alone.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// lateBinder answers empty until the Nth call, standing in for a binding
// written after the child was already woken.
func lateBinder(readyOn int) (func(context.Context) ([]SlotDatum, error), *int) {
	calls := 0
	return func(context.Context) ([]SlotDatum, error) {
		calls++
		if calls < readyOn {
			return nil, nil
		}
		return []SlotDatum{{Slot: "ledger", TagID: "tag-1", Content: "restated Q4 accrual"}}, nil
	}, &calls
}

func TestResolveBoundSlotsWaiting_LateBinding(t *testing.T) {
	t.Run("awaitBind waits for a binding still in flight: the datum arrives", func(t *testing.T) {
		resolve, calls := lateBinder(3)
		l := &Loop{ResolveBoundSlots: resolve}

		got, err := l.resolveBoundSlotsWaiting(context.Background(), nil, true)

		assert.NoError(t, err)
		assert.Len(t, got, 1, "the slot bound on the third read must be returned")
		assert.Equal(t, "restated Q4 accrual", got[0].Content)
		assert.Equal(t, 3, *calls, "must re-read until the binding appears, not a fixed number of times")
	})

	t.Run("without awaitBind it looks exactly once: the late datum is missed", func(t *testing.T) {
		// This is the pre-fix behavior, kept as a test so the retry cannot be
		// removed silently: one look, and a binding 200ms behind the wake is
		// simply not there.
		resolve, calls := lateBinder(3)
		l := &Loop{ResolveBoundSlots: resolve}

		got, err := l.resolveBoundSlotsWaiting(context.Background(), nil, false)

		assert.NoError(t, err)
		assert.Empty(t, got)
		assert.Equal(t, 1, *calls, "a resume with nothing in flight must not pay the wait")
	})

	t.Run("a datum already delivered does not restart the wait", func(t *testing.T) {
		// The retry keys on UNDELIVERED content, not on a non-empty result. A
		// child resuming a second time still holds its first datum, and
		// treating that as "the binding arrived" would return before the
		// second one landed — the exact bug, one exchange later.
		resolve, calls := lateBinder(1) // ready immediately, but already delivered
		l := &Loop{ResolveBoundSlots: resolve, deliveredSlotTags: map[string]bool{"tag-1": true}}

		got, err := l.resolveBoundSlotsWaiting(context.Background(), nil, true)

		assert.NoError(t, err)
		assert.Len(t, got, 1, "the resolve still reports the slot; it is the delivery that dedups")
		assert.Equal(t, slotBindRetries+1, *calls,
			"an already-delivered tag must not satisfy the wait for a new one")
	})

	t.Run("a tag in the transcript counts as delivered across a restart", func(t *testing.T) {
		resolve, calls := lateBinder(1)
		prior := []memory.Turn{{Content: []memory.ContentBlock{{Text: slotRefMarker("tag-1")}}}}
		l := &Loop{ResolveBoundSlots: resolve}

		_, err := l.resolveBoundSlotsWaiting(context.Background(), prior, true)

		assert.NoError(t, err)
		assert.Equal(t, slotBindRetries+1, *calls,
			"the durable delivery record must satisfy the same test as the in-process one")
	})
}
