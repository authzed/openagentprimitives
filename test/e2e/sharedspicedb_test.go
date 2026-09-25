//go:build e2e

package e2e_test

import (
	"os"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/platform/deplogs"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

func TestMain(m *testing.M) {
	// The harness runs component code IN-PROCESS — it constructs the operator's
	// reconcilers, channelsd's pipeline and the runner's loop directly rather
	// than exec'ing the built binaries — so none of their main()s, and none of
	// their deplogs.Silence() calls, ever run here. Without this the gate's logs
	// are buried in SpiceDB's schema-compiler trace JSON, which is precisely
	// where the fleet-wide gap was first visible. TestMain is this harness's
	// main(): one place, before any test, single-threaded.
	deplogs.Silence()

	code := m.Run()
	testspicedb.StopShared()
	os.Exit(code)
}
