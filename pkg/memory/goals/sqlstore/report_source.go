package sqlstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
)

func (s *Store) SourceGoal(ctx context.Context, namespace, domain, id string) (goals.Goal, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT payload FROM oap_goals WHERE domain=? AND id=?`), domain, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return goals.Goal{}, goals.ErrNotFound
	}
	if err != nil {
		return goals.Goal{}, err
	}
	g, err := decode(raw)
	if err != nil {
		return g, err
	}
	if g.Domain.Namespace != namespace || g.Domain.ID() != domain {
		return goals.Goal{}, goals.ErrDenied
	}
	return g, nil
}
