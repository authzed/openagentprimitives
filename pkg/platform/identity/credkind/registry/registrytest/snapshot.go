// Package registrytest provides shared credkind registry test helpers, so
// packages whose tests need to Reset() the registry (to probe an
// unregistered-type or empty-registry failure mode) don't each hand-roll the
// same save/restore.
package registrytest

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

// Snapshot saves the registry's current contents (typically populated by the
// caller's own credkind/imports blank import) and restores exactly that
// snapshot on cleanup. A bare registry.Reset() in a test's own cleanup would
// leave the registry empty for whatever test runs next in the same test
// binary — callers that need to Reset() the registry mid-test (e.g. to
// register a fake Kind, or to assert an unregistered-type error) call
// Snapshot(t) first so the registry is restored for every other test sharing
// the binary, regardless of execution order.
func Snapshot(t *testing.T) {
	t.Helper()
	saved := registry.All()
	t.Cleanup(func() {
		registry.Reset()
		for _, k := range saved {
			registry.Register(k)
		}
	})
}
