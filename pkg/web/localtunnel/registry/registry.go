// Package registry is the process-wide tunnel-provider registry: it maps a
// PublicEndpointSpec.Provider name (e.g. "ngrok") to the Factory that builds
// a localtunnel.Tunnel for it, so pkg/controllers/publicendpoint can resolve
// spec.provider without branching on its value.
//
// Storage, mutexing, sorting and panic-on-duplicate live in
// pkg/x/kindregistry — the same shape channelkinds, credkind and every other
// kind registry in this repo use. This one differs only in what it stores:
// Factory is a bare func with no method to derive a key from (unlike
// channelkinds.Kind.Name or credkind.Kind.Type), so registrations are kept
// as (name, Factory) pairs instead of keying off the registered value
// itself.
//
// Each provider package registers itself from an init(): ngrok
// (pkg/web/localtunnel/ngrok) is the production provider and its blank
// import belongs in the operator binary; stub (pkg/web/localtunnel/stub) is
// a test double and is deliberately never blank-imported into a production
// binary — see stub.go's doc comment for why.
package registry

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/web/localtunnel"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// Factory yields a FRESH Tunnel per call. One tunnel per PublicEndpoint:
// a shared instance would let two reconciles fight over one provider session.
type Factory func(opts Options) localtunnel.Tunnel

// Options is what every provider needs and nothing more. A provider needing
// something else takes it from its own package-level config, not from here —
// this struct is the contract every provider shares.
type Options struct {
	// AuthToken is the provider credential, read from the Secret named by
	// PublicEndpointSpec.AuthTokenRef. Never logged.
	AuthToken string
	// ReservedDomain pins a stable hostname; empty means provider-assigned.
	ReservedDomain string
}

// entry pairs a Factory with the name it was registered under, giving
// kindregistry something self-describing to key on (a bare func has no
// method of its own to extract a name from).
type entry struct {
	name    string
	factory Factory
}

var reg = kindregistry.New[entry]("localtunnel", func(e entry) string { return e.name })

// Register adds f under name, panicking on an empty or duplicate name — both
// are programmer errors caught at process start, the same as every other
// kind registry in this repo.
func Register(name string, f Factory) {
	reg.Register(entry{name: name, factory: f})
}

// Get returns the Factory registered under name.
//
// It is FAIL-CLOSED on both an unknown and an empty name, and the two cases
// name themselves distinctly in the returned error so a caller doesn't need
// to source-dive to tell "nothing configured" from "typo'd the value it
// configured." PublicEndpointSpec.Provider carries
// +kubebuilder:validation:MinLength=1, so the apiserver refuses an empty
// value on write, but a caller working from a zero-valued spec in memory
// (a stale cache read, a test fixture) can still reach Get(""); letting that
// silently resolve to some default provider would open the wrong tunnel.
// This mirrors cloud.For and credkind's registry.Get.
func Get(name string) (Factory, error) {
	if name == "" {
		return nil, fmt.Errorf("localtunnel: empty provider name (registered: %s)", registered())
	}
	e, ok := reg.Get(name)
	if !ok {
		return nil, fmt.Errorf("localtunnel: unknown provider %q (registered: %s)", name, registered())
	}
	return e.factory, nil
}

// Names returns every registered provider name, sorted.
func Names() []string { return reg.Keys() }

// registered renders the registered names for an error message, so a typo is
// diagnosable from the error alone without source-diving.
func registered() string {
	names := Names()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
