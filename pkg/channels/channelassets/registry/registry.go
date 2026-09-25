// Package registry holds the global renderer registry. Each kind under
// pkg/channels/channelassets/<x>/ calls Register from an init(), so a blank
// import is enough to wire it into the artifactrender controller and
// runner-side capability negotiation.
//
// Storage, locking and the panic-on-dup semantics live in pkg/x/kindregistry.
// What stays here is renderer-specific: the OutputMIME deny-list validation,
// which must run before the renderer is stored, and MatchAssetCapability.
package registry

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// reg is the process-wide renderer registry, keyed by Renderer.Kind().
var reg = kindregistry.New[channelassets.Renderer]("channelassets", channelassets.Renderer.Kind)

// Register adds r to the registry. Panics on a duplicate Kind(), or on a
// denied OutputMIME when r.Delivery() == DeliveryStandalone; a BundledOnly
// renderer may produce denied MIMEs because its output is never served as a
// top-level browser document. Both validations run before the dup-check and
// insert are delegated, so an invalid renderer is never stored.
func Register(r channelassets.Renderer) {
	kind := r.Kind()
	if kind == "" {
		panic("channelassets: renderer with empty Kind()")
	}
	if r.Delivery() == channelassets.DeliveryStandalone {
		for _, mime := range r.OutputMIMEs() {
			if channelassets.IsDeniedOutputMIME(mime) {
				panic(fmt.Sprintf("channelassets: standalone renderer %q has denied OutputMIME %q",
					kind, mime))
			}
		}
	}
	reg.Register(r)
}

// ByKind returns the renderer registered under kind.
func ByKind(kind string) (channelassets.Renderer, bool) { return reg.Get(kind) }

// All returns a snapshot of every registered renderer. Returned slice
// is independent of the registry's internal map.
func All() []channelassets.Renderer { return reg.All() }

// Reset clears the registry. Test-only; never call from production code.
func Reset() { reg.Reset() }

// MatchAssetCapability reports whether a channel's capabilities authorize
// delivery of mime. "asset:<mime>" matches by exact equality; an
// "asset:<type>/*" wildcard matches any subtype EXCEPT a denied one, so
// carrying a denied MIME always takes an explicit, audit-visible opt-in.
//
// Used by the runner-side meta-tool gating (does this channel accept the
// renderer's OutputMIME?) and by callers verifying an agent request before
// dispatch.
func MatchAssetCapability(caps []string, mime string) bool {
	for _, c := range caps {
		if !strings.HasPrefix(c, "asset:") {
			continue
		}
		spec := strings.TrimPrefix(c, "asset:")
		if spec == mime {
			return true
		}
		if strings.HasSuffix(spec, "/*") {
			prefix := strings.TrimSuffix(spec, "/*") + "/"
			if strings.HasPrefix(mime, prefix) && !channelassets.IsDeniedOutputMIME(mime) {
				return true
			}
		}
	}
	return false
}
