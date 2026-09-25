package categories

// init populates the process-wide interaction registry. Every binary that
// renders or decides an interaction blank-imports this package.
func init() { RegisterAll() }

// RegisterAll registers every prompt and notice category.
//
// Exported so a test that calls channelinteractions.Reset can put the registry
// back; an init cannot be re-invoked, so without this a single Reset leaves
// every later test looking at an empty registry — a failure that depends on
// test ordering and does not reproduce when the affected test runs alone.
//
// Production code must not call this: the init above already has, and
// registration panics on a duplicate name.
func RegisterAll() {
	registerPrompts()
	registerNotices()
}
