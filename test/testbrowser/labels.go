// This file carries NO build tag, unlike the rest of the package, so tooling
// that never links the fixture itself — the magefile, a reaper — can still name
// the labels it filters on instead of copying the strings. Mirrors
// test/testspicedb/labels.go exactly; only LabelFixtureValue differs so a
// browser-fixture container is never confused with a spicedb one.
package testbrowser

// Docker labels stamped onto every container this fixture starts, so a counter
// or a reaper can tell a suite's headless-Chrome container from a Chromium the
// developer runs on the same machine. LabelRunID is empty unless EnvRunID is
// set; set it to scope containers to a single `go test` invocation.
const (
	LabelFixture      = "com.authzed.agentprimitives.testfixture"
	LabelFixtureValue = "browser"
	LabelRunID        = "com.authzed.agentprimitives.testrun"
	EnvRunID          = "AP_TEST_RUN_ID"

	// LabelOwnerPID carries the PID of the test process that started the
	// container, so a later run can tell an abandoned container from one a
	// concurrently-running suite is still using. See reapAbandoned.
	LabelOwnerPID = "com.authzed.agentprimitives.testowner"

	// LabelOwnerStart carries a token identifying the OWNER PROCESS INSTANCE,
	// not just its number, so a recycled PID cannot masquerade as a live owner.
	// See test/testspicedb/labels.go for the full rationale — a PID alone is
	// not an identity on a box that recycles them quickly.
	LabelOwnerStart = "com.authzed.agentprimitives.testownerstart"
)
