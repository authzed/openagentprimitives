//go:build integration

// TestMain tears down the shared spicedb container this package's semantic
// tests boot. Without it the container outlives the run and `mage
// test:integration` reports "leaked spicedb container(s)" — the schema
// semantics file (schema_semantics_integration_test.go) calls
// testspicedb.SharedEndpoint but the package had no TestMain to purge it.
//
// Tagged `integration` deliberately: the unit-suite companion (schema_test.go)
// pins the schema TEXT and touches no container, so the unit tier must not
// acquire a TestMain it has no use for.
package schema_test

import (
	"os"
	"testing"

	"github.com/authzed/openagentprimitives/test/testspicedb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testspicedb.StopShared()
	os.Exit(code)
}
