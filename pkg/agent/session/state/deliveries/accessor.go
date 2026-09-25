package deliveries

import (
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

func init() {
	state.Register(deliveriesKind{})
}

// deliveriesKind is the state.Kind impl for the "deliveries" discriminator.
type deliveriesKind struct{}

func (deliveriesKind) Name() string                             { return "deliveries" }
func (deliveriesKind) NewStore(deps state.Deps) tool.StateStore { return NewStore(deps) }

// TryFrom returns the deliveries Store for the session, or (nil, false) when
// the Kind was not registered (a minimal-context session or a test fixture).
//
// There is no panicking From twin, unlike plans: both callers — respond_to_user
// on the write side and the artifact-delivered completion requirement on the
// read side — must keep working on a session that carries no delivery state,
// each in its own direction (the writer skips, the reader refuses).
func TryFrom(sess *tool.SessionContext) (*Store, bool) {
	if sess == nil || sess.State == nil {
		return nil, false
	}
	s, ok := sess.State.Get("deliveries")
	if !ok {
		return nil, false
	}
	st, ok := s.(*Store)
	return st, ok
}
