//go:build integration

package guardian_test

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
