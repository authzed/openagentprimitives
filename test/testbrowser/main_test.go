//go:build integration || e2e

package testbrowser

import (
	"os"
	"testing"
)

// TestMain purges the shared headless-Chrome container immediately after this
// package's tests finish, instead of leaving it for the idleGrace timer (which
// often cannot fire before the test process exits) or the 20-minute Expire
// backstop. This is the "a package that DOES define TestMain should call
// StopShared after m.Run()" path SharedChrome's doc and test/testspicedb call
// out — for a package with its own fixture-using tests, prompt cleanup belongs
// here rather than relying on the next run's reapAbandoned sweep.
func TestMain(m *testing.M) {
	code := m.Run()
	StopShared()
	os.Exit(code)
}
