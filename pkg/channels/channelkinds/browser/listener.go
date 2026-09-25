package browser

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/clienthosted"
)

// browserListener is the host-side Listener the browser chat UI drives. Every
// method is clienthosted.Listener's: the inbound paths are NATS request/publish
// only and share their behaviour with the `local` TUI kind verbatim. Nothing
// here is host-specific, so nothing is overridden — the kind name it carries is
// what makes an error name this surface.
type browserListener struct {
	clienthosted.Listener
}

// compile-time interface check.
var _ channelkinds.Listener = (*browserListener)(nil)
