// Package registry holds the global WebUI registry. UIs register at init()
// via Register; internal/cmd/webd blank-imports the UI packages and calls All().
// Storage/mutex/sort live in pkg/x/kindregistry; this package keeps the
// WebUI-specific panic messages by validating before delegating.
package registry

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// reg is the process-wide WebUI registry, keyed by WebUI.Name().
var reg = kindregistry.New[webui.WebUI]("webui", webui.WebUI.Name)

// Register adds u to the global registry. Panics on empty or duplicate
// Name(). The empty/dup checks are performed here so the panic messages match
// the WebUI-specific wording callers and tests expect; the actual insert is
// delegated to the shared registry.
func Register(u webui.WebUI) {
	if u.Name() == "" {
		panic("webui.Register: empty WebUI Name()")
	}
	if _, exists := reg.Get(u.Name()); exists {
		panic(fmt.Sprintf("webui.Register: duplicate WebUI name %q", u.Name()))
	}
	reg.Register(u)
}

// All returns every registered WebUI, sorted by Name.
func All() []webui.WebUI { return reg.All() }

// Reset clears the registry. Test-only.
func Reset() { reg.Reset() }
