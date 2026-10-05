package sqlstore

import (
	"context"
	"encoding/json"
	"errors"

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
func (s *Store) Unadmitted(ctx context.Context, id string, limit int) ([]sessionevents.Observation, error) {
	if limit < 1 || limit > 100 {
		return nil, sessionevents.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT o.payload FROM oap_session_observations o JOIN oap_event_subscription_sources s ON s.source_key=o.source_key WHERE s.subscription_id=? AND NOT EXISTS(SELECT 1 FROM oap_event_admissions a WHERE a.subscription_id=s.subscription_id AND a.observation_id=o.id) ORDER BY o.id LIMIT ?`), id, limit)
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

var _ sessionevents.RoutingStore = (*Store)(nil)
