//go:build integration

package agentclass_test

import (
	"os"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

func TestMain(m *testing.M) {
	code := testenv.RunPackage(m)
	testspicedb.StopShared()
	os.Exit(code)
}
