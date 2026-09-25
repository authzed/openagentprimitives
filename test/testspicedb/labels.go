// This file carries NO build tag, unlike the rest of the package, so tooling
// that never links the fixture itself — the magefile, a reaper — can still name
// the labels it filters on instead of copying the strings.
package testspicedb

// Docker labels stamped onto every container this fixture starts, so a counter
// or a reaper can tell a suite's container from a SpiceDB the developer runs on
// the same machine. LabelRunID is empty unless EnvRunID is set; set it to scope
// containers to a single `go test` invocation.
const (
	LabelFixture      = "com.authzed.agentprimitives.testfixture"
	LabelFixtureValue = "spicedb"
	LabelRunID        = "com.authzed.agentprimitives.testrun"
	EnvRunID          = "AP_TEST_RUN_ID"

	// LabelOwnerPID carries the PID of the test process that started the
	// container, so a later run can tell an abandoned container from one a
	// concurrently-running suite is still using. See reapAbandoned.
	LabelOwnerPID = "com.authzed.agentprimitives.testowner"

	// LabelOwnerStart carries a token identifying the OWNER PROCESS INSTANCE,
	// not just its number, so a recycled PID cannot masquerade as a live owner.
	//
	// A PID alone is not an identity. macOS recycles PIDs quickly and a suite
	// spawns dozens of short-lived test binaries, so a dead owner's number is
	// routinely reassigned to something still running — and the reaper, seeing
	// a live PID, then skips that container on every later sweep, forever. That
	// is the difference between a leak that plateaus and one that grows.
	LabelOwnerStart = "com.authzed.agentprimitives.testownerstart"
)
