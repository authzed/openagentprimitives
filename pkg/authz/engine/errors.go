package engine

import "github.com/authzed/openagentprimitives/pkg/authz"

// ErrNotYetMigrated re-exports the authz package sentinel for callers
// that only import pkg/authz/engine.
var ErrNotYetMigrated = authz.ErrNotYetMigrated
