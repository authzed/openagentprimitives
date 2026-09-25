package runner

import (
	"context"
	"sort"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

const (
	// operationActivityActivation gates which operations appear in the live
	// operation-activity tree: only operations that have been running at
	// least this long are surfaced, so short-lived operations never flicker
	// into view. Sibling knob to progressActivationDelay (progress.go).
	operationActivityActivation = 2 * time.Second

	// maxOperationActivityDepth bounds how deep the surfaced operation tree
	// nests (a plan-item-anchored or un-parented operation is depth 0).
	// Operations deeper than this are dropped from the payload rather than
	// growing it unbounded — a by-design display cap, not an error.
	maxOperationActivityDepth = 4

	// maxOperationActivityCalls bounds how many of an operation's most
	// recent tool calls are surfaced; older calls are dropped — a by-design
	// display cap, not an error.
	maxOperationActivityCalls = 8
)

// buildOperationActivity maps a snapshot of the live operation registry
// (tool.OperationRegistry.All()) into the gated, render-ready
// channelevents.OperationActivityPayload. Pure — a deterministic function of
// (ops, now, activation), hence table-testable and safe on every heartbeat tick.
//
// Operations are almost never Closed, so "open" alone would accumulate every
// operation the agent ever began. Included iff !Closed AND still doing current
// work: at least one in-flight call (CompletedAt.IsZero()), or no calls
// recorded yet (just opened, dispatch pending). One whose every call has
// completed is an orphan and is dropped. The activation gate runs on the
// OPERATION's CreatedAt, not on an in-flight call's age, so an op stays
// surfaced across back-to-back sequential calls instead of blinking out
// between them.
//
// Nodes sort stably by (depth, CreatedAt) — depth walks the Parent.OperationID
// chain; plan-item-anchored and un-parented operations are depth 0.
// Registry.All() yields nondeterministic map order and the compact line
// resolves to the LAST node, so the CreatedAt tie-break is what stops
// equal-depth operations flickering between ticks. Display caps drop depth
// beyond maxOperationActivityDepth, completed calls, and all but the newest
// maxOperationActivityCalls; every surfaced call is Active.
//
// Returns active=false (zero payload) when nothing is included; callers use
// that to publish a Cleared tick instead of a snapshot.
func buildOperationActivity(ops []tool.Operation, now time.Time, activation time.Duration) (channelevents.OperationActivityPayload, bool) {
	byID := make(map[string]tool.Operation, len(ops))
	for _, op := range ops {
		byID[op.ID] = op
	}

	depthCache := make(map[string]int, len(ops))
	var depthOf func(op tool.Operation, guard int) int
	depthOf = func(op tool.Operation, guard int) int {
		if d, ok := depthCache[op.ID]; ok {
			return d
		}
		if op.Parent == nil || op.Parent.PlanItem != nil {
			depthCache[op.ID] = 0
			return 0
		}
		// guard bounds recursion against a malformed (cyclic) parent chain —
		// defensive only; the registry does not knowingly produce cycles.
		if guard > len(ops)+1 {
			return 0
		}
		parent, ok := byID[op.Parent.OperationID]
		if !ok {
			depthCache[op.ID] = 0
			return 0
		}
		d := 1 + depthOf(parent, guard+1)
		depthCache[op.ID] = d
		return d
	}

	type candidate struct {
		op    tool.Operation
		depth int
	}
	var included []candidate
	for _, op := range ops {
		if op.Closed {
			continue
		}
		// The session root anchors the audit graph; it is not work the agent
		// declared, so it never surfaces here. Skipped BEFORE the current-work
		// test on purpose: a call-less operation counts as having current work,
		// so a freshly-minted root would otherwise pass that test and sit in
		// the tree, contentless, for the whole session.
		if op.Root {
			continue
		}
		inFlight := inFlightCalls(op.Calls)
		hasCurrentWork := len(inFlight) > 0 || len(op.Calls) == 0
		if !hasCurrentWork {
			continue // orphan: every call this op ever made has completed
		}
		// Gate on the OPERATION's age, not the in-flight call's: an op with
		// current work has been active since it was opened, so once past the
		// gate it stays surfaced instead of blinking out for `activation` in the
		// gap between one call completing and the next aging past it. (Gating on
		// min(oldest-in-flight-call.At, CreatedAt) collapses to CreatedAt anyway
		// — a call can never predate its operation.)
		if now.Sub(op.CreatedAt) < activation {
			continue
		}
		depth := depthOf(op, 0)
		if depth > maxOperationActivityDepth {
			continue // by-design display cap on tree depth
		}
		included = append(included, candidate{op: op, depth: depth})
	}
	if len(included) == 0 {
		return channelevents.OperationActivityPayload{}, false
	}

	// Sort ascending by (depth, CreatedAt): shallow (parent) nodes precede
	// deep (child) nodes, per the ordering invariant this payload's consumers
	// rely on; the CreatedAt tie-break makes the order deterministic across
	// ticks despite Registry.All()'s nondeterministic map order.
	sort.SliceStable(included, func(i, j int) bool {
		if included[i].depth != included[j].depth {
			return included[i].depth < included[j].depth
		}
		return included[i].op.CreatedAt.Before(included[j].op.CreatedAt)
	})

	nodes := make([]channelevents.OperationActivityNode, 0, len(included))
	for _, c := range included {
		nodes = append(nodes, operationActivityNode(c.op, now))
	}

	return channelevents.OperationActivityPayload{
		Operations:  nodes,
		CompactLine: channelevents.OperationActivityCompactLine(nodes),
	}, true
}

// inFlightCalls returns the subset of calls that have not yet completed
// (CompletedAt.IsZero()), preserving their relative (chronological) order.
func inFlightCalls(calls []tool.OperationCall) []tool.OperationCall {
	var out []tool.OperationCall
	for _, c := range calls {
		if c.CompletedAt.IsZero() {
			out = append(out, c)
		}
	}
	return out
}

// operationActivityNode maps one open, past-the-gate, currently-working
// operation into its render node. Every node returned by this function
// represents an included (hence active) operation, so Active is
// unconditionally true; every surfaced call is also Active — node.Calls only
// ever contains in-flight calls (see inFlightCalls).
func operationActivityNode(op tool.Operation, now time.Time) channelevents.OperationActivityNode {
	node := channelevents.OperationActivityNode{
		ID:             op.ID,
		Description:    op.Description,
		ElapsedSeconds: int(now.Sub(op.CreatedAt).Seconds()),
		Active:         true,
	}
	if op.Parent != nil {
		switch {
		case op.Parent.PlanItem != nil:
			// tool.PlanItemRef only carries (Plan, Item); the channel-facing
			// PlanItemRef is richer (label/status/details) but those live on
			// the plan_update snapshot itself — this is a correlation
			// pointer, not a duplicate of the plan item's render state.
			node.PlanItem = &channelevents.PlanItemRef{ID: op.Parent.PlanItem.Item}
		case op.Parent.OperationID != "":
			node.ParentOpID = op.Parent.OperationID
		}
	}

	calls := inFlightCalls(op.Calls)
	if len(calls) > maxOperationActivityCalls {
		// by-design display cap: keep only the newest calls
		calls = calls[len(calls)-maxOperationActivityCalls:]
	}
	if len(calls) > 0 {
		node.Calls = make([]channelevents.OperationActivityCallNode, len(calls))
		for i, call := range calls {
			node.Calls[i] = channelevents.OperationActivityCallNode{
				Tool:           call.Tool,
				Reason:         call.Reason,
				ElapsedSeconds: int(now.Sub(call.At).Seconds()),
				Active:         true,
			}
		}
	}
	return node
}

// runOperationActivityHeartbeat ticks the gated live operation-subtree
// snapshot every interval until ctx is done, publishing via
// l.OperationActivityPublish. Unlike progressReporter it needs no pause/resume
// around human waits: channelsd's watchdog already suppresses
// operation-activity forwarding while the session is yielded
// (EvOperationActivity in pkg/channels/channelsd/watchdog).
//
// Seq is a local monotonic counter, NOT PackSeq(memTurnIndex, blockIndex):
// that needs l.lastAssistantTurnIndex, read only from the main loop goroutine
// (loop.go), so reading it here would be a genuine data race.
// KindOperationActivity is render-only and exempt from the cross-kind Seq
// stale-drop ordering, so a self-contained counter is a race-free substitute.
//
// The caller (Loop.Run) guards on l.OperationActivityPublish != nil &&
// l.Operations != nil before starting this goroutine.
func (l *Loop) runOperationActivityHeartbeat(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	seq := 0
	wasActive := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			seq++
			payload, active := buildOperationActivity(l.Operations.All(), time.Now(), operationActivityActivation)
			switch {
			case active:
				l.OperationActivityPublish(payload, seq)
				wasActive = true
			case wasActive:
				l.OperationActivityPublish(channelevents.OperationActivityPayload{Cleared: true}, seq)
				wasActive = false
			}
		}
	}
}
