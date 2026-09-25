package hold

import (
	"context"
	"errors"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Trippers fans one signal out to several containment judgements.
//
// Setup holds exactly one Tripper because the Kind's ScopeHooks resolve one,
// and that is the right shape for the seam — but there is more than one reason
// to freeze a session, and they are INDEPENDENT. The denial streak asks "is
// this session repeatedly attempting what it may not do"; the trifecta closure
// asks "has this delegation acquired a dangerous combination". Neither implies
// the other, and a closure can go trifecta without ever producing a denial.
//
// ONE FAILING TRIPPER MUST NOT DISABLE THE OTHERS, which is the whole reason
// this exists rather than a slice inline at the call site. These are
// containment controls: if the streak tripper cannot read memory, that is a
// reason to log and keep going, not a reason the trifecta judgement silently
// stops running. Every tripper is invoked, every error is collected, and the
// aggregate is returned so the caller still learns something went wrong.
//
// A nil element is skipped rather than panicking: the operator declares
// trippers as interface values that stay nil when their feature is off (the
// threshold-zero case), and requiring every call site to pre-filter would make
// the typed-nil hazard everyone's problem instead of this function's.
func Trippers(ts ...Tripper) Tripper {
	live := make([]Tripper, 0, len(ts))
	for _, t := range ts {
		if t != nil {
			live = append(live, t)
		}
	}
	switch len(live) {
	case 0:
		// Nil, not an empty composite: Setup(nil) is the documented
		// "no tripper" state and NewScopeHooks returns a no-op for it. An
		// empty composite would be a non-nil interface that does nothing,
		// which reads as wired in every log and diagnostic.
		return nil
	case 1:
		return live[0]
	}
	return multiTripper(live)
}

type multiTripper []Tripper

func (m multiTripper) Name() string {
	names := make([]string, 0, len(m))
	for _, t := range m {
		names = append(names, t.Name())
	}
	return strings.Join(names, "+")
}

func (m multiTripper) OnSignal(ctx context.Context, sig memory.Signal) error {
	var errs []error
	for _, t := range m {
		if err := t.OnSignal(ctx, sig); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
