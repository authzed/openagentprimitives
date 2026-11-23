package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
)

func (s *Store) ActiveSubscriptions(ctx context.Context, after string, limit int) ([]sessionevents.SubscriptionState, error) {
	if limit < 1 || limit > 100 {
		return nil, sessionevents.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT id FROM oap_event_subscriptions WHERE stopped=0 AND id>? ORDER BY id LIMIT ?`), after, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var result []sessionevents.SubscriptionState
	for _, id := range ids {
		sub, err := s.Subscription(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, sub)
	}
	return result, nil
}

func (s *Store) Unadmitted(ctx context.Context, id string, now time.Time, limit int) ([]sessionevents.Observation, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return nil, sessionevents.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT o.payload FROM oap_event_route_pending p JOIN oap_session_observations o ON o.id=p.observation_id WHERE p.subscription_id=? AND p.observed_at<=? ORDER BY p.observed_at,p.observation_id LIMIT ?`), id, timestamp(now), limit)
	if err != nil {
		return nil, err
	}
	var result []sessionevents.Observation
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		var observation sessionevents.Observation
		if err := json.Unmarshal([]byte(raw), &observation); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		result = append(result, observation)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

// enqueueExisting builds the routing index when a watch is first created or
// the schema is upgraded. Runtime polling reads only the unadmitted backlog,
// never the growing archive. Future observations remain indexed until due.
func (s *Store) enqueueExisting(ctx context.Context, tx *sql.Tx, subscriptionID string) error {
	afterSub, afterObservation := "", ""
	for {
		rows, err := tx.QueryContext(ctx, s.query(`SELECT x.subscription_id,o.id,o.payload FROM oap_event_subscription_sources x JOIN oap_event_subscriptions s ON s.id=x.subscription_id JOIN oap_session_observations o ON o.source_key=x.source_key WHERE s.stopped=0 AND (?='' OR x.subscription_id=?) AND (x.subscription_id>? OR (x.subscription_id=? AND o.id>?)) AND NOT EXISTS(SELECT 1 FROM oap_event_admissions a WHERE a.subscription_id=x.subscription_id AND a.observation_id=o.id) ORDER BY x.subscription_id,o.id LIMIT 100`), subscriptionID, subscriptionID, afterSub, afterSub, afterObservation)
		if err != nil {
			return err
		}
		type pending struct{ sub, id, at string }
		var page []pending
		for rows.Next() {
			var item pending
			var raw string
			if err = rows.Scan(&item.sub, &item.id, &raw); err != nil {
				return errors.Join(err, rows.Close())
			}
			var observation sessionevents.Observation
			if err = json.Unmarshal([]byte(raw), &observation); err != nil {
				return errors.Join(err, rows.Close())
			}
			item.at = timestamp(observation.ObservedAt)
			page = append(page, item)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, item := range page {
			if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO oap_event_route_pending(subscription_id,observation_id,observed_at) VALUES(?,?,?) ON CONFLICT(subscription_id,observation_id) DO NOTHING`), item.sub, item.id, item.at); err != nil {
				return err
			}
			afterSub, afterObservation = item.sub, item.id
		}
	}
}

var _ sessionevents.RoutingStore = (*Store)(nil)
