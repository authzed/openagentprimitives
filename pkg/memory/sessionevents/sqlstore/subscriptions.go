package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
)

var _ sessionevents.TriggerStore = (*Store)(nil)

func (s *Store) beginEvents(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE oap_session_observation_lock SET generation=generation+1 WHERE id=1`); err != nil {
		rollback(tx)
		return nil, err
	}
	return tx, nil
}
func (s *Store) subscription(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (sessionevents.SubscriptionState, error) {
	var state sessionevents.SubscriptionState
	var payload string
	var stopped int
	err := q.QueryRowContext(ctx, s.query(`SELECT payload,used,stopped,stop_reason FROM oap_event_subscriptions WHERE id=?`), id).Scan(&payload, &state.Used, &stopped, &state.StopReason)
	if errors.Is(err, sql.ErrNoRows) {
		return state, sessionevents.ErrNotFound
	}
	if err != nil {
		return state, err
	}
	state.Stopped = stopped != 0
	return state, json.Unmarshal([]byte(payload), &state.Subscription)
}
func (s *Store) Subscription(ctx context.Context, id string) (sessionevents.SubscriptionState, error) {
	return s.subscription(ctx, s.db, id)
}
func (s *Store) CreateSubscription(ctx context.Context, sub sessionevents.Subscription) (sessionevents.SubscriptionState, error) {
	if err := sub.Validate(); err != nil {
		return sessionevents.SubscriptionState{}, err
	}
	sub.StartsAt = sub.StartsAt.UTC()
	sub.EndsAt = sub.EndsAt.UTC()
	digest, err := sub.Digest()
	if err != nil {
		return sessionevents.SubscriptionState{}, err
	}
	payload, err := json.Marshal(sub)
	if err != nil {
		return sessionevents.SubscriptionState{}, err
	}
	tx, err := s.beginEvents(ctx)
	if err != nil {
		return sessionevents.SubscriptionState{}, err
	}
	defer rollback(tx)
	var previous string
	err = tx.QueryRowContext(ctx, s.query(`SELECT digest FROM oap_event_subscriptions WHERE id=?`), sub.ID).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return sessionevents.SubscriptionState{}, err
	}
	if err == nil && previous != digest {
		return sessionevents.SubscriptionState{}, sessionevents.ErrConflict
	}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_event_subscriptions(id,digest,payload,used,stopped,stop_reason) VALUES(?,?,?,0,0,'')`), sub.ID, digest, string(payload)); err != nil {
			return sessionevents.SubscriptionState{}, err
		}
	}
	state, err := s.subscription(ctx, tx, sub.ID)
	if err != nil {
		return state, err
	}
	return state, tx.Commit()
}

func timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func (s *Store) Admit(ctx context.Context, id string, source sessionevents.Source, eventID string, now time.Time) (sessionevents.Admission, error) {
	var admission sessionevents.Admission
	if source.Validate() != nil || eventID == "" || now.IsZero() {
		return admission, sessionevents.ErrInvalid
	}
	tx, err := s.beginEvents(ctx)
	if err != nil {
		return admission, err
	}
	defer rollback(tx)
	state, err := s.subscription(ctx, tx, id)
	if err != nil {
		return admission, err
	}
	sub := state.Subscription
	if source != sub.Source {
		return admission, sessionevents.ErrDenied
	}
	var payload string
	err = tx.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_session_observations WHERE source_key=? AND event_id=?`), source.Key(), eventID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return admission, sessionevents.ErrNotFound
	}
	if err != nil {
		return admission, err
	}
	var observation sessionevents.Observation
	if err = json.Unmarshal([]byte(payload), &observation); err != nil {
		return admission, err
	}
	var prior string
	err = tx.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_event_admissions WHERE subscription_id=? AND observation_id=?`), id, observation.ID()).Scan(&prior)
	if err == nil {
		if err = json.Unmarshal([]byte(prior), &admission); err != nil {
			return admission, err
		}
		return admission, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return admission, err
	}
	// Future observations are retryable after their timestamp, rather than being
	// assigned a sliding deadline or permanently skipped before they occur.
	if observation.ObservedAt.After(now) {
		return admission, sessionevents.ErrInvalid
	}
	admission = sessionevents.Admission{SubscriptionID: id, ObservationID: observation.ID(), AcceptedAt: now.UTC()}
	switch {
	case state.Stopped:
		admission.Disposition = "stopped"
	case !sub.Predicate.Matches(observation):
		admission.Disposition = "unmatched"
	case now.Before(sub.StartsAt) || !now.Before(sub.EndsAt) || observation.ObservedAt.Before(sub.StartsAt) || !observation.ObservedAt.Before(sub.EndsAt):
		admission.Disposition = "outside_window"
	case state.Used >= sub.MaxRuns:
		admission.Disposition = "exhausted"
	default:
		deadline := observation.ObservedAt.Add(time.Duration(sub.RunWindowSeconds) * time.Second)
		if deadline.After(sub.EndsAt) {
			deadline = sub.EndsAt
		}
		if !deadline.After(now) {
			admission.Disposition = "expired"
		} else {
			window, ok, err := sessionschedule.EventWindow(sub.Timezone, sub.QuietHours, now, deadline)
			if err != nil {
				return admission, err
			}
			if !ok {
				admission.Disposition = "quiet_expired"
			} else {
				// Expiry is durable, so an expired launch cannot block a fresh matching
				// event or be revived after the consumer reconnects.
				if _, err = tx.ExecContext(ctx, s.query(`UPDATE oap_event_launches SET state='expired' WHERE subscription_id=? AND state='pending' AND expires_at<=?`), id, timestamp(now)); err != nil {
					return admission, err
				}
				var pending int
				if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM oap_event_launches WHERE subscription_id=? AND state='pending'`), id).Scan(&pending); err != nil {
					return admission, err
				}
				if pending > 0 {
					admission.Disposition = "burst_skipped"
				} else {
					admission.Disposition = "accepted"
					admission.Window = window
					// Hash the exact immutable watch and observation identities; timestamp
					// changes on retry cannot create another launch or spend another run.
					admission.LaunchID = sessionevents.LaunchID(sub.ID, observation.ID())
					launch := sessionevents.Launch{Admission: admission, Subscription: sub, Observation: observation}
					launchJSON, err := json.Marshal(launch)
					if err != nil {
						return admission, err
					}
					if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_event_launches(id,subscription_id,observation_id,due_at,expires_at,state,payload) VALUES(?,?,?,?,?,'pending',?)`), admission.LaunchID, id, observation.ID(), timestamp(window.DueAt), timestamp(window.ExpiresAt), string(launchJSON)); err != nil {
						return admission, err
					}
					if _, err = tx.ExecContext(ctx, s.query(`UPDATE oap_event_subscriptions SET used=used+1 WHERE id=?`), id); err != nil {
						return admission, err
					}
				}
			}
		}
	}
	encoded, err := json.Marshal(admission)
	if err != nil {
		return admission, err
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_event_admissions(subscription_id,observation_id,payload) VALUES(?,?,?)`), id, observation.ID(), string(encoded)); err != nil {
		return admission, err
	}
	return admission, tx.Commit()
}
func (s *Store) Pending(ctx context.Context, now time.Time, limit int) ([]sessionevents.Launch, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return nil, sessionevents.ErrInvalid
	}

	tx, err := s.beginEvents(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE oap_event_launches SET state='expired' WHERE state='pending' AND expires_at<=?`), timestamp(now)); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, s.query(`SELECT l.payload FROM oap_event_launches l JOIN oap_event_subscriptions s ON s.id=l.subscription_id WHERE l.state='pending' AND s.stopped=0 AND l.due_at<=? AND l.expires_at>? ORDER BY l.due_at,l.id LIMIT ?`), timestamp(now), timestamp(now), limit)
	if err != nil {
		return nil, err
	}
	// Always close before committing; return close failures, including early
	// decode/scan exits, instead of hiding a failed query cleanup.
	var launches []sessionevents.Launch
	for rows.Next() {
		var payload string
		if err = rows.Scan(&payload); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		var launch sessionevents.Launch
		if err = json.Unmarshal([]byte(payload), &launch); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		launches = append(launches, launch)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return launches, tx.Commit()
}

func (s *Store) Acknowledge(ctx context.Context, id string) error {
	tx, err := s.beginEvents(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var state string
	err = tx.QueryRowContext(ctx, s.query(`SELECT state FROM oap_event_launches WHERE id=?`), id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionevents.ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != "pending" && state != "acknowledged" {
		return sessionevents.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE oap_event_launches SET state='acknowledged' WHERE id=?`), id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) StopSubscription(ctx context.Context, id, reason string) error {
	if strings.TrimSpace(reason) == "" || len(reason) > 1024 || !utf8.ValidString(reason) {
		return sessionevents.ErrInvalid
	}
	tx, err := s.beginEvents(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	state, err := s.subscription(ctx, tx, id)
	if err != nil {
		return err
	}
	if state.Stopped {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE oap_event_subscriptions SET stopped=1,stop_reason=? WHERE id=?`), reason, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.query(`UPDATE oap_event_launches SET state='cancelled' WHERE subscription_id=? AND state='pending'`), id); err != nil {
		return fmt.Errorf("cancel event launches: %w", err)
	}
	return tx.Commit()
}

func (s *Store) Launch(ctx context.Context, id string) (sessionevents.Launch, error) {
	var launch sessionevents.Launch
	var payload, state string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT payload,state FROM oap_event_launches WHERE id=?`), id).Scan(&payload, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return launch, sessionevents.ErrNotFound
	}
	if err != nil {
		return launch, err
	}
	if state != "pending" {
		return launch, sessionevents.ErrDenied
	}
	return launch, json.Unmarshal([]byte(payload), &launch)
}
