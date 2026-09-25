// Package spicedb re-exports the canonical SpiceDB authorization schema for the
// oap CLI (apply-schema + install's bootstrap ConfigMap). The single source of
// truth is pkg/authz/spicedb/schema; this package only re-exports it so oap's existing
// call sites (spicedb.Schema) keep working without each importing the authz
// package directly.
package spicedb

import authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"

// Schema is the canonical agentprimitives SpiceDB schema (pkg/authz/spicedb/schema).
var Schema = authzschema.Schema
