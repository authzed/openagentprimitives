package sqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
)

func occurrenceWindowMatches(g goals.Goal, o goals.Occurrence) bool {
	if g.Execution == nil {
		return false
	}
	if g.Execution.Terms.Event == nil {
		return o.Event == nil && g.Execution.Terms.ContainsWindow(o.DueAt, o.ExpiresAt)
	}
	if o.Event == nil {
		return false
	}
	return eventMatches(g, *o.Event) && o.DueAt.Equal(o.Event.Admission.Window.DueAt) && o.ExpiresAt.Equal(o.Event.Admission.Window.ExpiresAt)
}

func eventMatches(g goals.Goal, launch sessionevents.Launch) bool {
	sub, err := goals.EventSubscription(g)
	a := launch.Admission
	o := launch.Observation
	return err == nil && reflect.DeepEqual(sub, launch.Subscription) && o.Validate() == nil && o.Source == sub.Source && sub.Predicate.Matches(o) &&
		a.SubscriptionID == sub.ID && a.ObservationID == o.ID() && a.LaunchID == sessionevents.LaunchID(sub.ID, o.ID()) && a.Disposition == "accepted" &&
		!o.ObservedAt.Before(sub.StartsAt) && o.ObservedAt.Before(sub.EndsAt) && !a.Window.DueAt.Before(a.AcceptedAt) &&
		!a.Window.DueAt.Before(sub.StartsAt) && a.Window.ExpiresAt.After(a.Window.DueAt) && !a.Window.ExpiresAt.After(sub.EndsAt) &&
		!a.Window.ExpiresAt.After(o.ObservedAt.Add(time.Duration(sub.RunWindowSeconds)*time.Second))
}

// MaterializeEvent is trusted internal persistence. The consumer must authorize
// the canonical pending launch first. Goal identity, evidence and audit intent
// commit together; retries after a lost outbox acknowledgment retain one root.
func (s *Store) MaterializeEvent(ctx context.Context, g goals.Goal, launch sessionevents.Launch, now time.Time) (goals.Occurrence, error) {
	if g.Execution == nil {
		return goals.Occurrence{}, goals.ErrDenied
	}
	return s.dispatchTx(ctx, func(tx *sql.Tx) (goals.Occurrence, error) {
		current, err := s.lockedGoal(ctx, tx, g.Domain, g.ID)
		if err != nil {
			return goals.Occurrence{}, err
		}
		if !executionMatches(current, g.Revision, g.Execution.Digest) || !eventMatches(current, launch) {
			return goals.Occurrence{}, goals.ErrDenied
		}
		h := sha256.Sum256([]byte(launch.Admission.LaunchID))
		id := "occ-" + hex.EncodeToString(h[:])
		prior, err := s.occurrence(ctx, tx, id)
		if err == nil {
			if !reflect.DeepEqual(prior.Event, &launch) {
				return prior, goals.ErrConflict
			}
			return prior, nil
		}
		if err != goals.ErrNotFound {
			return prior, err
		}
		w := launch.Admission.Window
		if now.Before(w.DueAt) || !now.Before(w.ExpiresAt) {
			return prior, goals.ErrDenied
		}
		owner := goals.Domain{Namespace: g.Domain.Namespace, Owner: g.Domain.Owner}.ID()
		class := goals.Domain{Namespace: g.Domain.Namespace, ClassUID: g.Domain.ClassUID}.ID()
		_, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_goal_occurrences(id,domain,owner_key,class_key,goal_id,goal_revision,consent_digest,due_at,expires_at,state,session_name) VALUES(?,?,?,?,?,?,?,?,?,?,?)`), id, g.Domain.ID(), owner, class, g.ID, g.Revision, g.Execution.Digest, w.DueAt.UnixNano(), w.ExpiresAt.UnixNano(), string(goals.OccurrenceQueued), "goal-"+hex.EncodeToString(h[:16]))
		if err != nil {
			return prior, err
		}
		payload, err := json.Marshal(launch)
		if err != nil {
			return prior, err
		}
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_goal_run_events(occurrence_id,launch_id,payload) VALUES(?,?,?)`), id, launch.Admission.LaunchID, string(payload)); err != nil {
			return prior, err
		}
		o, err := s.occurrence(ctx, tx, id)
		if err != nil {
			return o, err
		}
		return o, s.auditOccurrence(ctx, tx, o, "execution_event_accepted", now)
	})
}

var _ goals.EventOccurrenceStore = (*Store)(nil)
