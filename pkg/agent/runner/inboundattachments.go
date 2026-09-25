package runner

import (
	"context"
	"log/slog"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// inboundAttachmentWaitBudget is how long awaitInboundAttachmentTurn waits for
// the inbox turn carrying turn 0's attachments, and inboundAttachmentPoll is
// how often it re-reads memory looking for it. Package-level vars, not consts,
// so tests can shrink both without waiting out the production budget.
//
// The budget is the shared constant on purpose — see its doc for why the fence
// and the work it fences must not carry independently chosen numbers.
var (
	inboundAttachmentWaitBudget = spiceboxv1alpha1.InboundAttachmentWriteBudget
	inboundAttachmentPoll       = 50 * time.Millisecond
)

// awaitInboundAttachmentTurn blocks until the "inbox"-role turn carrying this
// session's first-message attachments is visible in memory, or the budget
// expires. It is the runner's half of the fence documented on
// AnnotationInboundAttachmentCount.
//
// Why it is needed: an inbound message that creates a session arrives split in
// two. channelsd puts the message TEXT in spec.Prompt (placed as turn 0 by
// Run's cold-start path) and can only write the attachments afterwards, as an
// inbox turn, once it has fetched each file from the channel's CDN and pushed
// it through the operator's /inbound-asset route. The operator spawns this
// runner off the AgentSession's mere existence, so without this wait the loop
// reaches drainInbox — and then the model — while that write is still in
// flight. The user's file is then absent from the very turn that asked about
// it, and lands a turn late, after the agent has already answered blind.
//
// It waits for ANY inbox turn, not specifically an undrained one. The
// question it asks is "did the write happen", and drainInbox answers that
// durably for all time by preserving the raw inbox turn and marking it
// consumed with a paired inbox_done rather than deleting it. heldInbox's
// filter is the wrong predicate here: a consumed turn is proof the write
// landed, not a reason to keep waiting for it.
//
// Never returns an error and never fails the session. Expiry means the writer
// died or its append failed — both already logged on that side — and answering
// the user late-but-blind beats not answering at all. Expiry is logged here
// and, on a channel-attached session, said out loud to the user, because a
// reply that quietly ignores the file they just uploaded is the confusing
// outcome this whole path exists to avoid.
func (l *Loop) awaitInboundAttachmentTurn(ctx context.Context) {
	if l.AgentSession == nil {
		return
	}
	count, present := spiceboxv1alpha1.InboundAttachmentCount(l.AgentSession.Annotations)
	if !present {
		return
	}
	session := l.SessionKey.Namespace + "/" + l.SessionKey.Name

	deadline := time.Now().Add(inboundAttachmentWaitBudget)
	waited := time.Duration(0)
	for {
		seen, err := l.hasInboxTurn(ctx)
		if err != nil {
			// A memory read that fails here fails for drainInbox moments later,
			// which DOES surface it. Log and stop waiting rather than burning
			// the budget re-reading a store that is down.
			slog.Default().Info("inbound-attachment wait: memory read failed; proceeding without it",
				"session", session, "attachments", count, "err", err.Error())
			return
		}
		if seen {
			if waited > 0 {
				slog.Default().Info("inbound-attachment wait: attachment turn arrived",
					"session", session, "attachments", count, "waited", waited.String())
			}
			return
		}
		if !time.Now().Before(deadline) {
			slog.Default().Info("inbound-attachment wait: budget expired; answering without the attachments",
				"session", session, "attachments", count, "budget", inboundAttachmentWaitBudget.String())
			if l.Notify != nil {
				l.Notify(ctx, "I couldn't get hold of the attached files, so I'm answering without them.")
			}
			return
		}
		select {
		case <-ctx.Done():
			slog.Default().Info("inbound-attachment wait: context done; proceeding without the attachments",
				"session", session, "attachments", count, "err", ctx.Err().Error())
			return
		case <-time.After(inboundAttachmentPoll):
			waited += inboundAttachmentPoll
		}
	}
}

// hasInboxTurn reports whether the session's memory holds any "inbox"-role
// turn, drained or not. Deliberately coarser than heldInbox, which filters out
// consumed entries — see awaitInboundAttachmentTurn for why the consumed ones
// are the point.
func (l *Loop) hasInboxTurn(ctx context.Context) (bool, error) {
	all, err := l.Memory.ReadAll(ctx)
	if err != nil {
		return false, err
	}
	for _, t := range all {
		if t.Role == "inbox" {
			return true, nil
		}
	}
	return false, nil
}
