package openingsummary

import (
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

func init() { state.Register(openingSummaryKind{}) }

type openingSummaryKind struct{}

func (openingSummaryKind) Name() string                             { return KindName }
func (openingSummaryKind) NewStore(deps state.Deps) tool.StateStore { return NewStore(deps) }

// TryFrom returns the opening-summary Store for the session, or (nil, false).
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
