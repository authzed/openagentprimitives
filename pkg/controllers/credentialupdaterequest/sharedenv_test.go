//go:build integration

package credentialupdaterequest_test

import (
	"os"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// TestMain boots one apiserver per package, and — since the agent-owned seam
// tests answer agentidentity#update_credential against a REAL SpiceDB —
// tears down the shared SpiceDB container on the way out. Without the
// StopShared, a package that calls testspicedb.SharedEndpoint leaks its
// container past the run; `mage test:integration` warns about exactly that.
func TestMain(m *testing.M) {
	code := testenv.RunPackage(m)
	testspicedb.StopShared()
	os.Exit(code)
}
