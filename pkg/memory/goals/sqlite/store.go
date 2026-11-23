package sqlite

import (
	"database/sql"

	"github.com/authzed/openagentprimitives/pkg/memory/goals/sqlstore"
)

func New(db *sql.DB) *sqlstore.Store { return sqlstore.New(db, false) }
