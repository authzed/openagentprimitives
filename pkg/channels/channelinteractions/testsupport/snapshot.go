// Package testsupport gives tests a snapshot/restore of the interaction
// category registry and its decision bindings, so a test that registers or
// binds a category never leaves the init()-registered production rows wiped
// for the next test.
package testsupport

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// WithRegistrySnapshot captures every registered category, then restores
// exactly that set (via Reset + re-Register) and clears bindings on cleanup.
// Bindings cannot be enumerated, so ResetBindings is the restore for them —
// callers that need production bindings re-Bind inside the test.
func WithRegistrySnapshot(t *testing.T) {
	t.Helper()
	saved := channelinteractions.All()
	t.Cleanup(func() {
		channelinteractions.Reset()
		channelinteractions.ResetBindings()
		for _, c := range saved {
			channelinteractions.Register(c)
		}
	})
}
