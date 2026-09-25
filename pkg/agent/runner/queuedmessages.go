package runner

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// awaitResumeDrainRetries and awaitResumeDrainRetryBackoff bound the
// read-visibility retry drainAwaitResume performs (total worst-case wait ≈
// retries × backoff). Package-level (not const) so tests can shorten the wait.
var (
	awaitResumeDrainRetries      = 5
	awaitResumeDrainRetryBackoff = 100 * time.Millisecond
)

// heldInbox returns the not-yet-drained "inbox"-role turns (those with no
// paired "inbox_done" marker at the same Index), sorted ascending by Index.
// Read-only: it neither places "user" turns nor writes markers. drainInbox
// uses it for its read/filter/sort prologue; the yield-boundary sites use it
// to decide whether anything is queued.
func (l *Loop) heldInbox(ctx context.Context) ([]memory.Turn, error) {
	all, err := l.Memory.ReadAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("held inbox: read memory: %w", err)
	}
	done := map[int]bool{}
	for _, t := range all {
		if t.Role == "inbox_done" {
			done[t.Index] = true
		}
	}
	held := make([]memory.Turn, 0)
	for _, t := range all {
		if t.Role == "inbox" && !done[t.Index] {
			held = append(held, t)
		}
	}
	sort.Slice(held, func(i, j int) bool { return held[i].Index < held[j].Index })
	return held, nil
}

// idleWithWakeRecheck parks the session at phase=Idle and then closes the
// post-Idle append window. It is the ONLY way a channel-attached loop should
// reach Idle.
//
// The window: channelsd appends an inbound, then decides whether the operator
// must respawn an exited runner. If it reads a phase from before this runner
// idled, it leaves the delivery to a NATS wakeup that this already-exiting pod
// never receives, and the turn strands in memory as an undrained inbox entry —
// the user gets silence until they happen to send another message.
//
// The ordering is what makes the pair airtight, and is why the re-read must come
// AFTER WriteIdle. Runner: write Idle → read inbox. channelsd: append inbox →
// read phase. Either the append preceded this read (we see it and request the
// respawn), or it followed this read and therefore followed WriteIdle
// committing, so channelsd read phase=Idle and annotates. No interleaving leaves
// the turn invisible to both; a double request collapses to one wake via
// RequestWake's already-pending skip.
//
// A failure here is logged, never returned: the session IS correctly Idle, and
// turning a missed wake into a Failed phase would replace a delayed reply with a
// dead session. The held turn still drains on the user's next message.
func (l *Loop) idleWithWakeRecheck(ctx context.Context, reason string) error {
	if err := l.Status.WriteIdle(ctx, reason); err != nil {
		return err
	}
	session := l.SessionKey.Namespace + "/" + l.SessionKey.Name
	held, err := l.heldInbox(ctx)
	if err != nil {
		slog.Default().Info("post-idle inbox re-check failed; a message that landed during the idle transition may wait for the user's next reply",
			"session", session, "err", err.Error())
		return nil
	}
	if len(held) == 0 {
		return nil
	}
	req, err := l.Status.RequestWake(ctx)
	if err != nil {
		slog.Default().Info("post-idle wake request failed; held inbound will not be picked up until the user's next reply",
			"session", session, "held", len(held), "err", err.Error())
		return nil
	}
	slog.Default().Info("inbound landed as this runner idled",
		"session", session, "held", len(held), "outcome", string(req.Outcome), "phase", req.Phase)
	return nil
}

// resultsIncludeAwaitResume reports whether the just-finished tool batch was a
// parking tool's resume — await_user_message woken by a user reply, or
// ask_parent woken by the delegating agent's answer. Only then does the
// mid-loop site drain the held inbox; ordinary tool work holds it.
func resultsIncludeAwaitResume(results []tool.Result) bool {
	for _, r := range results {
		if r.AwaitResumed {
			return true
		}
	}
	return false
}

// drainHeldAtYield places the held inbox turns at a yield boundary (via the
// unchanged drainInbox placement) and best-effort announces the pickup. It is
// the shared drain used by both yield sites (await-resume and agent_work_
// complete-with-queue). The announce is transient (l.Notify), not a durable
// channel message.
//
// The announcement counts only what the PERSON sent. The same inbox also
// carries operator notices — pkg/controllers/inboxwake writes one straight
// into it when a person's own test of a built agent starts, pauses or ends,
// with no Author, because nobody sent it — and "Picking up 1 message you
// sent." in answer to one of those describes something the person did not do.
func (l *Loop) drainHeldAtYield(ctx context.Context, nextIndex int) ([]memory.Turn, int, error) {
	placed, next, err := l.drainInbox(ctx, nextIndex)
	if err != nil {
		return placed, next, err
	}
	sent := 0
	for _, t := range placed {
		if !t.Author.Empty() {
			sent++
		}
	}
	if sent > 0 && l.Notify != nil {
		noun := "message"
		if sent != 1 {
			noun = "messages"
		}
		l.Notify(ctx, fmt.Sprintf("Picking up %d %s you sent.", sent, noun))
	}
	return placed, next, nil
}

// drainAwaitResume drains queued inbound human messages at an
// await_user_message resume boundary, retrying the read to defeat a
// read-after-write visibility race.
//
// The resume was triggered by a real inbound: channelsd wrote the message as an
// "inbox" turn to memory and THEN published the NATS wake that unblocked
// await_user_message. So an empty held-inbox on the first read after a resume
// almost always means that write is not yet visible — NOT that there is nothing
// to drain — and empty is otherwise indistinguishable from "already drained".
// Unretried, the reply is silently stranded, so we re-read a bounded number of
// times before giving up.
//
// Still empty after the bound: log loudly and return empty rather than hanging
// or failing the session. A genuinely spurious wake is recoverable, and a
// real-but-missed message is re-checked by the next yield's drain (heldInbox
// re-evaluates every still-undrained inbox turn), so nothing is lost permanently
// — but an operator can see the anomaly. Never silent.
func (l *Loop) drainAwaitResume(ctx context.Context, nextIndex int) ([]memory.Turn, int, error) {
	placed, drained, err := l.drainHeldAtYield(ctx, nextIndex)
	if err != nil || len(placed) > 0 {
		return placed, drained, err
	}
	for attempt := 1; attempt <= awaitResumeDrainRetries; attempt++ {
		select {
		case <-ctx.Done():
			return placed, drained, ctx.Err()
		case <-time.After(awaitResumeDrainRetryBackoff):
		}
		placed, drained, err = l.drainHeldAtYield(ctx, nextIndex)
		if err != nil || len(placed) > 0 {
			return placed, drained, err
		}
	}
	slog.Default().Info("await_user_message resumed but held inbox drained nothing after retries; a reply may be stranded until the next yield re-checks it",
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
		"retries", awaitResumeDrainRetries)
	return placed, drained, nil
}
