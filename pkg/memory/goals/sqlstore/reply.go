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
	if err := reply.Intent.Validate(); err != nil {
		return goals.Occurrence{}, err
	}
	p := reply.Intent.Payload
	if strings.TrimSpace(p.Text) == "" || len(p.Text) > 16000 || len(p.Attachments) != 0 || p.Opening != nil {
		return goals.Occurrence{}, goals.ErrInvalid
	}
	reply.State, reply.Receipt = "prepared", nil
	if len(reply.Sources) == 0 {
		reply.Sources = nil
	}
	return s.dispatchTx(ctx, func(tx *sql.Tx) (goals.Occurrence, error) {
		current, err := s.occurrence(ctx, tx, o.ID)
		if err != nil {
			return current, err
		}
		if current.SessionUID == "" || current.SessionUID != o.SessionUID || current.GoalRevision != o.GoalRevision || current.ConsentDigest != o.ConsentDigest || current.State != goals.OccurrenceRunning || current.Outcome != nil || !now.Before(current.ExpiresAt) || reply.Intent.Payload.Delivery.SessionUID != current.SessionUID || reply.Intent.Session.Namespace != current.Domain.Namespace || reply.Intent.Session.Name != current.SessionName {
			return current, goals.ErrConflict
		}
		g, err := s.lockedGoal(ctx, tx, current.Domain, current.GoalID)
		if err != nil {
			return current, err
		}
		if !executionMatches(g, current.GoalRevision, current.ConsentDigest) {
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
		dest := reply.Intent.Destination
		pin := g.Execution.Terms.Destination
		if dest.Recipient != current.Domain.Owner || dest.ChannelUID != pin.ChannelUID || dest.BindingDigest != pin.BindingDigest {
			return current, goals.ErrDenied
		}
		if current.Reply != nil {
			reply.Intent.CreatedAt = current.Reply.Intent.CreatedAt
			if !reflect.DeepEqual(reply.Intent, current.Reply.Intent) || !reflect.DeepEqual(reply.Sources, current.Reply.Sources) {
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
		return changed, s.auditOccurrence(ctx, tx, changed, "reply_prepared", now)
	})
}

func (s *Store) AttemptReply(ctx context.Context, o goals.Occurrence, now time.Time) (goals.Occurrence, error) {
	return s.fenced(ctx, o, now, "reply_attempted", func(tx *sql.Tx, current goals.Occurrence) error {
		if current.Reply == nil || current.Reply.State != "prepared" || current.State != goals.OccurrenceRunning || current.Outcome != nil || !now.Before(current.ExpiresAt) {
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
		reply.State = "attempted"
		return s.saveReply(ctx, tx, o.ID, reply)
	})
}

func (s *Store) ConcludeReply(ctx context.Context, o goals.Occurrence, receipt *delivery.Receipt, now time.Time) (goals.Occurrence, error) {
	return s.fenced(ctx, o, now, "reply_reconciled", func(tx *sql.Tx, current goals.Occurrence) error {
		if current.Reply == nil || (current.Reply.State == "prepared" && receipt == nil) {
			return goals.ErrConflict
		}
		reply := *current.Reply
		if receipt != nil {
			if err := receipt.Validate(reply.Intent); err != nil {
				return err
			}
		}
		if reply.State == "accepted" || reply.State == "absent" {
			if !reflect.DeepEqual(reply.Receipt, receipt) {
				return goals.ErrConflict
			}
			return nil
		}
		reply.State, reply.Receipt = "absent", receipt
		if receipt != nil {
			reply.State = "accepted"
		}
		return s.saveReply(ctx, tx, o.ID, reply)
	})
}
