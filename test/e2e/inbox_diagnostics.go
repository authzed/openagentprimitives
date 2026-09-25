//go:build e2e

package e2e

import (
	"fmt"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// describeInboxTurns reports whether each inbound human message was drained by
// a runner.
//
// "A turn produced nothing" has two causes that look identical from outside and
// need opposite investigations: channelsd never delivered the message, or it
// delivered it and no runner ever consumed it. The state dump could not
// distinguish them, so the reader had to guess — and guessing wrong on the
// centerdot approval flake cost real time.
//
// The data already answers it. channelsd writes an "inbox" turn; the runner's
// drainInbox promotes it to a real user turn and writes an "inbox_done" marker
// at the SAME index, preserving the raw inbox turn because memory is
// append-only. So an inbox with no matching inbox_done arrived and was never
// consumed — a wake/respawn problem — while no inbox turn at all means nothing
// was ever delivered.
func describeInboxTurns(turns []memory.Turn) string {
	const (
		roleInbox     = "inbox"
		roleInboxDone = "inbox_done"
	)

	drained := map[int]bool{}
	var inbound []int
	for _, t := range turns {
		switch t.Role {
		case roleInboxDone:
			drained[t.Index] = true
		case roleInbox:
			inbound = append(inbound, t.Index)
		}
	}

	if len(inbound) == 0 {
		return "no inbound (mid-session) message was written at all — " +
			"channelsd never delivered it, so this is a delivery problem, not a wake problem"
	}

	var stuck []int
	for _, idx := range inbound {
		if !drained[idx] {
			stuck = append(stuck, idx)
		}
	}
	if len(stuck) == 0 {
		return fmt.Sprintf("all %d drained by a runner", len(inbound))
	}

	sort.Ints(stuck)
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d NOT drained by any runner", len(stuck), len(inbound))
	for _, idx := range stuck {
		fmt.Fprintf(&b, "\n  index=%d — written to memory, never consumed "+
			"(no runner was woken/respawned for it)", idx)
	}
	return b.String()
}
