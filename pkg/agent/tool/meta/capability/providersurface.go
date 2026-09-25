package capability

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// ProviderSurface is the optional half of Capability, declared by a capability
// whose tools act on a THIRD PARTY's own surface — the pull request's check
// run, not anything the agent runtime owns or can stand in for.
//
// It exists so a consumer can ask which assembled tools reach a provider
// without naming a capability or a tool. The steel-thread capture is that
// consumer: a tool answering on a provider's surface is a tool a fixture can
// only stand in for, so a replayed transcript that CALLED one is refused
// rather than quietly re-answered against nothing.
//
// Optional rather than a method on Capability because the answer is "no" for
// every capability but one today, and putting it on the interface would write
// that "no" into two dozen files with nothing to say about providers. A
// capability that does not implement it contributes nothing here.
//
// Whether a declaring capability contributes anything for a GIVEN session is
// still Offer's decision, not this interface's: trigger_status contributes
// nothing on a kind with no status surface, and ProviderSurfaceTools reports
// exactly what Offer returned.
type ProviderSurface interface {
	Capability

	// ActsOnProviderSurface marks the declaration. It takes and returns
	// nothing on purpose: a capability's tools either reach a provider or they
	// do not, and there is no per-session variation to express here — Offer
	// already decides whether the capability contributes at all.
	ActsOnProviderSurface()
}

// ProviderSurfaceTools returns the tools contributed, for these deps, by every
// ACTIVE capability that declares ProviderSurface.
//
// Resolved through the same offerOne that Assemble drives, so a tool listed
// here is a tool Assemble put in the merged list, decided by the same grant,
// the same parsed config and the same OfferContext. A second resolution path
// would let the two disagree about which tools a session was offered, and the
// disagreement would land in a bundle as a tool the replay believes it may
// call.
//
// deps.Logger sees the skips of the capabilities consulted here. A caller that
// has already assembled the same deps should pass a discard logger, or the
// operator reads every skip twice.
func ProviderSurfaceTools(ctx context.Context, deps AssembleDeps) []tool.Tool {
	var out []tool.Tool
	for _, c := range Ordered() {
		ps, ok := c.(ProviderSurface)
		if !ok {
			continue
		}
		if tools, _, offered := offerOne(ctx, ps, deps); offered {
			out = append(out, tools...)
		}
	}
	return out
}
