// Package registry is the process-wide credential-kind registry. The exported
// funcs are thin forwarders so the storage, mutexing, sorting and
// panic-on-duplicate live in pkg/x/kindregistry, exactly as the channelkinds
// and workspacekinds registries do.
package registry

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

var reg = kindregistry.New[credkind.Kind]("credkind", credkind.Kind.Type)

// Register adds a Kind, panicking on an empty or duplicate type. Called from
// each kind package's init().
func Register(k credkind.Kind) { reg.Register(k) }

// All returns every registered Kind, sorted by type. Used by invariant tests
// that must cover all kinds without hardcoding the list.
func All() []credkind.Kind { return reg.All() }

// Keys returns every registered type, sorted.
func Keys() []string { return reg.Keys() }

// Reset clears the registry. Test-only helper; do not call from production
// code paths.
func Reset() { reg.Reset() }

// Get returns the Kind for a credential type.
//
// It is FAIL-CLOSED on both an unknown and an empty type. Neither may fall
// back to a default: a credential whose type nothing handles must never
// resolve to a silent no-op, because the caller would then inject an empty
// value and the failure would surface far from its cause — as a 401 from some
// upstream API, or as an agent that mysteriously never dispatches. This
// mirrors cloud.For, which refuses an empty cluster kind for the same reason.
func Get(typ string) (credkind.Kind, error) {
	if typ == "" {
		return nil, fmt.Errorf("empty credential type (registered: %s)", registered())
	}
	k, ok := reg.Get(typ)
	if !ok {
		return nil, fmt.Errorf(
			"unknown credential type %q (registered: %s); the binary may be missing its blank import of pkg/platform/identity/credkind/imports",
			typ, registered())
	}
	return k, nil
}

// registered renders the registered types for an error message, so a typo is
// diagnosable from the error alone without source-diving.
func registered() string {
	keys := Keys()
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}
