package postgres

import (
	"github.com/authzed/openagentprimitives/pkg/memory/goals/sqlstore"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// The returned database/sql handle borrows the operator's existing pool.
func New(pool *pgxpool.Pool) *sqlstore.Store { return sqlstore.New(stdlib.OpenDBFromPool(pool), true) }
