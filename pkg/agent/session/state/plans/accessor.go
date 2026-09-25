package plans

import (
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

func init() {
	state.Register(plansKind{})
}

// plansKind is the state.Kind impl for the "plans" discriminator.
type plansKind struct{}

func (plansKind) Name() string                             { return "plans" }
func (plansKind) NewStore(deps state.Deps) tool.StateStore { return NewStore(deps) }

// From returns the *Store registered for this session. Panics when the
// session was constructed without the plans Kind registered (a
// programmer error in the runner setup, not a runtime situation tools
// should be defensive about).
func From(sess *tool.SessionContext) *Store {
	if sess == nil || sess.State == nil {
		panic("plans.From: SessionContext.State is nil")
	}
	s, ok := sess.State.Get("plans")
	if !ok {
		panic("plans.From: plans Kind is not registered for this session")
	}
	return s.(*Store)
}

// TryFrom returns the plans Store for the session, or (nil, false) when the
// plans Kind was not registered (kubectl-driven / minimal-context sessions).
// Unlike From it never panics — callers that only opportunistically touch
// plans (the loop's activity echoes) use this.
func TryFrom(sess *tool.SessionContext) (*Store, bool) {
	if sess == nil || sess.State == nil {
		return nil, false
	}
	s, ok := sess.State.Get("plans")
	if !ok {
		return nil, false
	}
	st, ok := s.(*Store)
	return st, ok
}
