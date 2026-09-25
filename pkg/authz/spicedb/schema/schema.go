// Package schema is the single source of truth for the agentprimitives SpiceDB
// authorization schema. The canonical text lives in schema.zed and is embedded
// here; every consumer (the operator's guardian composer, oap spicedb apply-schema /
// install, and the test SpiceDB fixtures) reads from this one place so the base
// definitions can never drift out of sync. There is intentionally no second
// copy — the SpiceDB pod's bootstrap ConfigMap is generated from this embed by
// oap install.
//
// It sits under pkg/authz/spicedb because a .zed schema is SpiceDB's own
// language, not a backend-neutral authorization concept: swapping the backend
// would mean re-expressing the model, not re-pointing an import. The sibling
// pkg/authz/spicedb (schema.go) PARSES schema text; this package IS the text.
package schema

import _ "embed"

// Schema is the embedded canonical schema text (schema.zed). It is a complete,
// standalone-valid SpiceDB schema: callers that bootstrap a fresh datastore
// (oap spicedb apply-schema, the SpiceDB pod, test fixtures) write it verbatim, while
// the operator's guardian composer uses it as the scaffold it concatenates the
// dynamic MCPServer / channel-kind fragments and grant relations onto.
//
//go:embed schema.zed
var Schema string
