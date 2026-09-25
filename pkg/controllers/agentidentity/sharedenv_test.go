//go:build integration

package agentidentity_test

import (
	"os"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

func TestMain(m *testing.M) {
	code := testenv.RunPackage(m)
	// The platform-link tests boot a shared SpiceDB container; stop it after
	// the apiserver so a package run leaves no container behind.
	testspicedb.StopShared()
	os.Exit(code)
}
