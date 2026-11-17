package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
)

var _ goals.ReplyStore = (*Store)(nil)

func (s *Store) saveReply(ctx context.Context, tx *sql.Tx, id string, reply goals.RunReply) error {
	raw, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_goal_run_replies(occurrence_id,operation_id,payload) VALUES(?,?,?) ON CONFLICT(occurrence_id) DO UPDATE SET payload=excluded.payload`), id, reply.Intent.Payload.Delivery.ID, string(raw))
	return err
}

func (s *Store) PrepareReply(ctx context.Context, o goals.Occurrence, reply goals.RunReply, now time.Time) (goals.Occurrence, error) {
	return s.recordReply(ctx, o, reply, now, goals.ReplyPrepared)
}

// HandOffReply records a group run's reply as already handed to the outbound
// relay: terminal, unconfirmed, without a receipt. Only a group goal may take
// this path; a private reply always needs a receiver's confirmation.
func (s *Store) HandOffReply(ctx context.Context, o goals.Occurrence, reply goals.RunReply, now time.Time) (goals.Occurrence, error) {
	return s.recordReply(ctx, o, reply, now, goals.ReplyUnconfirmed)
}

// recordReply is PrepareReply and HandOffReply: the same fences and
// destination check, recording the reply in state.
func (s *Store) recordReply(ctx context.Context, o goals.Occurrence, reply goals.RunReply, now time.Time, state string) (goals.Occurrence, error) {
	if err := reply.Intent.Validate(); err != nil {
		return goals.Occurrence{}, err
	}
	p := reply.Intent.Payload
	if strings.TrimSpace(p.Text) == "" || len(p.Text) > 16000 || len(p.Attachments) != 0 || p.Opening != nil {
		return goals.Occurrence{}, goals.ErrInvalid
	}
	reply.State, reply.Receipt = state, nil
	if len(reply.Sources) == 0 {
		reply.Sources = nil
	}
	return s.dispatchTx(ctx, func(tx *sql.Tx) (goals.Occurrence, error) {
		current, err := s.occurrence(ctx, tx, o.ID)
		if err != nil {
			return current, err
		}
		if !replyRunCurrent(current, o, now) || reply.Intent.Payload.Delivery.SessionUID != current.SessionUID || reply.Intent.Session.Namespace != current.Domain.Namespace || reply.Intent.Session.Name != current.SessionName {
			return current, goals.ErrConflict
		}
		g, err := s.lockedGoal(ctx, tx, current.Domain, current.GoalID)
		if err != nil {
			return current, err
		}
		g, bound := boundGoal(g, current.GoalRevision, current.ConsentDigest)
		if !bound {
			return current, goals.ErrConflict
		}
		permitted := false
		for _, op := range g.Execution.Terms.AllowedOperations {
			if op == "respond_to_user" {
				permitted = true
			}
		}
		if !permitted {
			return current, goals.ErrDenied
		}
		// The controller owns the kind decision (no receiver -> hand-off); the
		// store enforces only that a hand-off is never a private reply.
		if state == goals.ReplyUnconfirmed && !g.Domain.GroupOwned() {
			return current, goals.ErrDenied
		}
		if err := replyDestinationMatches(g, current, reply.Intent.Destination); err != nil {
			return current, err
		}
		if current.Reply != nil {
			// One reply per run. The same call repeated (same operation:
			// session UID, tool use and payload digest) gets the existing
			// record back, so a retried or resumed call can still publish;
			// the runner's delivered note keeps an already-published call
			// from publishing again. Anything else is a second reply, and a
			// reply never switches between the confirmed and hand-off paths.
			handOff := state == goals.ReplyUnconfirmed
			handedOff := current.Reply.State == goals.ReplyUnconfirmed || current.Reply.State == goals.ReplyPublishFailed
			reply.Intent.CreatedAt = current.Reply.Intent.CreatedAt
			if handOff != handedOff || !reflect.DeepEqual(reply.Intent, current.Reply.Intent) || !reflect.DeepEqual(reply.Sources, current.Reply.Sources) {
				if handedOff {
					return current, goals.ErrReplyHandedOff
				}
				return current, goals.ErrConflict
			}
			return current, nil
		}
		if err := s.saveReply(ctx, tx, o.ID, reply); err != nil {
			return current, err
		}
		changed, err := s.occurrence(ctx, tx, o.ID)
		if err != nil {
			return changed, err
		}
		action := "reply_prepared"
		if state == goals.ReplyUnconfirmed {
			action = "reply_unconfirmed"
		}
		return changed, s.auditOccurrence(ctx, tx, changed, action, now)
	})
}

// FailReplyPublish moves a handed-off reply to ReplyPublishFailed: the runner
// could not publish the envelope the relay would have posted. It takes the
// same run fences as the hand-off and names the operation it is about; a
// repeat for a reply already marked failed is a no-op.
func (s *Store) FailReplyPublish(ctx context.Context, o goals.Occurrence, operationID string, now time.Time) (goals.Occurrence, error) {
	return s.dispatchTx(ctx, func(tx *sql.Tx) (goals.Occurrence, error) {
		current, err := s.occurrence(ctx, tx, o.ID)
		if err != nil {
			return current, err
		}
		if !replyRunCurrent(current, o, now) || current.Reply == nil || current.Reply.Intent.Payload.Delivery == nil || current.Reply.Intent.Payload.Delivery.ID != operationID {
			return current, goals.ErrConflict
		}
		switch current.Reply.State {
		case goals.ReplyPublishFailed:
			return current, nil
		case goals.ReplyUnconfirmed:
		default:
			return current, goals.ErrConflict
		}
		g, err := s.lockedGoal(ctx, tx, current.Domain, current.GoalID)
		if err != nil {
			return current, err
		}
		if _, bound := boundGoal(g, current.GoalRevision, current.ConsentDigest); !bound {
			return current, goals.ErrConflict
		}
		reply := *current.Reply
		reply.State = goals.ReplyPublishFailed
		if err := s.saveReply(ctx, tx, o.ID, reply); err != nil {
			return current, err
		}
		changed, err := s.occurrence(ctx, tx, o.ID)
		if err != nil {
			return changed, err
		}
		return changed, s.auditOccurrence(ctx, tx, changed, "reply_publish_failed", now)
	})
}

// replyRunCurrent is the run fence every reply write takes: o is the attached
// session's still-running, unexpired run at the revision and consent it was
// bound to.
func replyRunCurrent(current, o goals.Occurrence, now time.Time) bool {
	return current.SessionUID != "" && current.SessionUID == o.SessionUID && current.GoalRevision == o.GoalRevision && current.ConsentDigest == o.ConsentDigest && current.State == goals.OccurrenceRunning && current.Outcome == nil && now.Before(current.ExpiresAt)
}

func (s *Store) AttemptReply(ctx context.Context, o goals.Occurrence, now time.Time) (goals.Occurrence, error) {
	return s.fenced(ctx, o, now, "reply_attempted", func(tx *sql.Tx, current goals.Occurrence) error {
		if current.Reply == nil || current.Reply.State != goals.ReplyPrepared || current.State != goals.OccurrenceRunning || current.Outcome != nil || !now.Before(current.ExpiresAt) {
			return goals.ErrConflict
		}
		g, err := s.lockedGoal(ctx, tx, current.Domain, current.GoalID)
		if err != nil {
			return err
		}
		if !executionMatches(g, current.GoalRevision, current.ConsentDigest) {
			return goals.ErrConflict
		}
		reply := *current.Reply
		reply.State = goals.ReplyAttempted
		return s.saveReply(ctx, tx, o.ID, reply)
	})
}

func (s *Store) ConcludeReply(ctx context.Context, o goals.Occurrence, receipt *delivery.Receipt, now time.Time) (goals.Occurrence, error) {
	return s.fenced(ctx, o, now, "reply_reconciled", func(tx *sql.Tx, current goals.Occurrence) error {
		if current.Reply == nil || (current.Reply.State == goals.ReplyPrepared && receipt == nil) {
			return goals.ErrConflict
		}
		reply := *current.Reply
		if receipt != nil {
			if err := receipt.Validate(reply.Intent); err != nil {
				return err
			}
		}
		if reply.State == goals.ReplyUnconfirmed || reply.State == goals.ReplyPublishFailed {
			// Handed to the relay without a receiver: there is no receipt to
			// conclude with, and none may be attached after the fact.
			return goals.ErrConflict
		}
		if reply.State == goals.ReplyAccepted || reply.State == goals.ReplyAbsent {
			if !reflect.DeepEqual(reply.Receipt, receipt) {
				return goals.ErrConflict
			}
			return nil
		}
		reply.State, reply.Receipt = goals.ReplyAbsent, receipt
		if receipt != nil {
			reply.State = goals.ReplyAccepted
		}
		return s.saveReply(ctx, tx, o.ID, reply)
	})
}

// replyDestinationMatches re-checks a reply's destination against the reviewed
// terms. A private reply goes to its owner on the pinned binding; a group
// reply goes to the goal's member group on its pinned Channel and route.
func replyDestinationMatches(g goals.Goal, current goals.Occurrence, dest delivery.Destination) error {
	if dest.Recipient != current.Domain.Owner {
		return goals.ErrDenied
	}
	if g.Domain.GroupOwned() {
		pin := g.Execution.Terms.Group
		if pin == nil || dest.Recipient != pin.MemberGroup || dest.ChannelUID != pin.ChannelUID {
			return goals.ErrDenied
		}
		route, err := pin.RouteDigest()
		if err != nil {
			return err
		}
		if dest.BindingDigest != route {
			return goals.ErrDenied
		}
		return nil
	}
	pin := g.Execution.Terms.Destination
	if dest.ChannelUID != pin.ChannelUID || dest.BindingDigest != pin.BindingDigest {
		return goals.ErrDenied
	}
	return nil
}
