//go:build e2e

package bronzethread_test

import (
	"os"
	"testing"

	"github.com/authzed/openagentprimitives/test/testbrowser"
)

// TestMain purges this binary's shared headless-Chrome container immediately
// after every bundle has run, mirroring test/e2e's own TestMain +
// testspicedb.StopShared shape (test/e2e/sharedspicedb_test.go).
//
// Without it the browser bundles' shared container relies entirely on
// testbrowser's idleGrace timer (SharedChrome's doc), which bridges the
// sub-millisecond gaps between serial tests within one binary but is not a
// guaranteed cleanup at process exit: a test binary that returns from m.Run()
// and calls os.Exit immediately may do so before a 5s AfterFunc ever fires,
// leaving the container to wait out dockertest's Expire(1200) backstop (or a
// later run's reap pass) instead of being purged now.
func TestMain(m *testing.M) {
	code := m.Run()
	testbrowser.StopShared()
	os.Exit(code)
}
