package channelinteractions

import (
	"context"
	"fmt"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// Decision is what a bound handler receives: the decision payload plus the
// cached original request when the pending-prompt cache still holds it
// (nil after a channelsd restart — handlers must tolerate that).
type Decision struct {
	Session channelevents.SessionRef
	Payload channelevents.InteractionDecisionPayload
	Request *channelevents.InteractionRequestPayload
}

// Outcome is a handler's verdict; the pipe turns it into the
// InteractionAppliedPayload published to both runner and surface.
type Outcome struct {
	Result      string // one of the channelevents.Outcome* consts
	OutcomeText string
	Reason      string
	MintedURL   string

	// Suppressed marks an ASYNC category whose real resolution is out-of-band
	// (queued_messages fires a KindInterruptRequest; the eventual
	// KindInterruptApplied is bridged back into interaction_applied). When
	// true the pipe MUST NOT publish the synchronous interaction_applied —
	// that would resolve the card before the interrupt landed. A suppressed
	// Outcome carries no Result, so Validate is skipped for it; the pipe still
	// dedupes the decision and clears the pending prompt.
	Suppressed bool
}

func (o Outcome) Validate() error {
	switch o.Result {
	case channelevents.OutcomeApproved, channelevents.OutcomeDenied,
		channelevents.OutcomeExpired, channelevents.OutcomeResolved:
		return nil
	default:
		return fmt.Errorf("interaction outcome: unknown result %q", o.Result)
	}
}

// DecisionHandler is the per-category plumbing that validates and applies a
// decision. Handlers close over their dependencies at Bind time — binding
// happens at process start (DI), keeping Category rows declarative.
type DecisionHandler func(ctx context.Context, d Decision) (Outcome, error)

var (
	bindMu   sync.RWMutex
	handlers = map[string]DecisionHandler{}
)

// Bind attaches the decision handler for a registered category. Unknown
// category, nil handler, and double-bind are programmer errors caught at
// process start.
func Bind(category string, h DecisionHandler) {
	if _, ok := Get(category); !ok {
		panic(fmt.Sprintf("channelinteractions: Bind of unregistered category %q", category))
	}
	if h == nil {
		panic(fmt.Sprintf("channelinteractions: Bind of nil handler for %q", category))
	}
	bindMu.Lock()
	defer bindMu.Unlock()
	if _, dup := handlers[category]; dup {
		panic(fmt.Sprintf("channelinteractions: duplicate Bind for %q", category))
	}
	handlers[category] = h
}

// HandlerFor returns the bound handler for a category, if any.
func HandlerFor(category string) (DecisionHandler, bool) {
	bindMu.RLock()
	defer bindMu.RUnlock()
	h, ok := handlers[category]
	return h, ok
}

// ResetBindings clears all bindings. Test-only helper.
func ResetBindings() {
	bindMu.Lock()
	defer bindMu.Unlock()
	handlers = map[string]DecisionHandler{}
}
