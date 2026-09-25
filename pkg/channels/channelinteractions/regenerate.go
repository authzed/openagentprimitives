package channelinteractions

import (
	"context"
	"fmt"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// Regenerator is the per-category plumbing bound for ResurfaceRegenerate
// categories: rather than republishing the cached request envelope verbatim,
// the publisher rebuilds it from scratch — credential links re-mint their
// signed URL and expiry window on every re-surface. Regenerators close over
// their dependencies and attach at process start via
// BindRegenerator/RegeneratorFor.
//
// cachedReq is nil after a channelsd restart flushed the in-memory
// pending-prompt cache, so a regenerator MUST tolerate that.
type Regenerator func(ctx context.Context, sess *v1alpha1.AgentSession, cachedReq *channelevents.InteractionRequestPayload) error

var (
	regenMu      sync.RWMutex
	regenerators = map[string]Regenerator{}
)

// BindRegenerator attaches the regenerator for a registered category.
// Unknown category, nil regenerator, and double-bind are programmer errors
// caught at process start.
func BindRegenerator(category string, r Regenerator) {
	if _, ok := Get(category); !ok {
		panic(fmt.Sprintf("channelinteractions: BindRegenerator of unregistered category %q", category))
	}
	if r == nil {
		panic(fmt.Sprintf("channelinteractions: BindRegenerator of nil regenerator for %q", category))
	}
	regenMu.Lock()
	defer regenMu.Unlock()
	if _, dup := regenerators[category]; dup {
		panic(fmt.Sprintf("channelinteractions: duplicate BindRegenerator for %q", category))
	}
	regenerators[category] = r
}

// RegeneratorFor returns the bound regenerator for a category, if any.
func RegeneratorFor(category string) (Regenerator, bool) {
	regenMu.RLock()
	defer regenMu.RUnlock()
	r, ok := regenerators[category]
	return r, ok
}

// ResetRegenerators clears all bindings. Test-only helper.
func ResetRegenerators() {
	regenMu.Lock()
	defer regenMu.Unlock()
	regenerators = map[string]Regenerator{}
}
