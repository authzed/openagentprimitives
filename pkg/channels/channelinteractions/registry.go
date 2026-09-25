package channelinteractions

import (
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// reg is the process-wide interaction-category registry, same shape as
// every other kind registry (see pkg/x/kindregistry).
var reg = kindregistry.New[Category]("channelinteractions", func(c Category) string { return c.Name })

// Register adds a category. It panics on an invalid category, an empty
// name, or a duplicate — programmer errors caught at init() time.
func Register(c Category) {
	if err := c.Validate(); err != nil {
		panic("channelinteractions: " + err.Error())
	}
	reg.Register(c)
}

func Get(name string) (Category, bool) { return reg.Get(name) }

func All() []Category { return reg.All() }

// RegenerateAtPark returns every registered category that parks a session at
// phase AND rebuilds its prompt via a regenerator rather than a stored copy.
//
// This is what makes a ResurfaceRegenerate prompt recoverable with no stored
// state. A regenerator reconstructs the whole prompt from live durable sources
// (credential_link's mints a fresh signed link from the SessionUserIdentity),
// so its only input is WHICH category the session is parked on — which the
// session's own phase already says. Asking the registry by phase, instead of
// iterating a cache of previously-sent prompts, is what lets the re-send
// survive a restart that emptied every cache, and what keeps the credential
// link out of storage: a fresh link per delivery, never a replayable one.
func RegenerateAtPark(phase string) []Category {
	if phase == "" {
		return nil
	}
	var out []Category
	for _, c := range reg.All() {
		if c.Park == phase && c.Resurface == ResurfaceRegenerate {
			out = append(out, c)
		}
	}
	return out
}

// Reset clears the registry. Test-only helper; do not call from production
// code paths.
func Reset() { reg.Reset() }
