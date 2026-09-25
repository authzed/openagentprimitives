package triggerstatus

import (
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

func init() {
	state.Register(triggerStatusKind{})
}

// triggerStatusKind is the state.Kind impl for the "triggerstatus"
// discriminator.
type triggerStatusKind struct{}

func (triggerStatusKind) Name() string                             { return KindName }
func (triggerStatusKind) NewStore(deps state.Deps) tool.StateStore { return NewStore(deps) }

// TryFrom returns the trigger-status Store for the session, or (nil, false)
// when the Kind was not registered (a minimal-context session or a test
// fixture).
//
// There is no panicking From twin, matching deliveries: both sides must keep
// working on a session carrying no trigger-status state, each in its own
// direction — the writing tools skip the record and say so in the log, and the
// reading completion requirement refuses rather than reporting a satisfaction
// it cannot establish.
func TryFrom(sess *tool.SessionContext) (*Store, bool) {
	if sess == nil || sess.State == nil {
		return nil, false
	}
	s, ok := sess.State.Get(KindName)
	if !ok {
		return nil, false
	}
	st, ok := s.(*Store)
	return st, ok
}
