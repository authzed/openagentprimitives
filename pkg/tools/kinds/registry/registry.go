// Package registry holds the global tool-kind registry. Each kind under
// pkg/tools/kinds/<x>/ calls Register from an init() so that importing
// the package is enough to wire it into the unified `oap tools` CLI.
package registry

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/tools/contract"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// reg is the process-wide tool-kind registry. Register/Get/All/Reset are thin
// forwarders onto it; DecodeAny is a tool-kind-specific extra layered on top.
var reg = kindregistry.New[contract.Kind]("tools/kinds", contract.Kind.Name)

// Register adds k to the registry. Panics on duplicate name.
func Register(k contract.Kind) { reg.Register(k) }

// Get returns the kind registered under name.
func Get(name string) (contract.Kind, bool) { return reg.Get(name) }

// All returns a snapshot of every registered kind, sorted by Name().
func All() []contract.Kind { return reg.All() }

// DecodeAny walks every registered kind asking it to DecodeYAML. Returns
// the first kind that produces a non-nil object. If a kind returns an
// error it is propagated immediately. If no kind claims the doc, returns
// a descriptive error listing which kinds were tried.
func DecodeAny(doc []byte) (client.Object, contract.Kind, error) {
	all := All()
	tried := make([]string, 0, len(all))
	for _, k := range all {
		obj, err := k.DecodeYAML(doc)
		if err != nil {
			return nil, nil, fmt.Errorf("decode as %s: %w", k.Name(), err)
		}
		if obj != nil {
			return obj, k, nil
		}
		tried = append(tried, k.Name())
	}
	if len(tried) == 0 {
		return nil, nil, fmt.Errorf("no tool kinds registered")
	}
	return nil, nil, fmt.Errorf("no registered tool kind claimed the document (tried: %v)", tried)
}

// Reset clears the registry. Test-only helper; do not call from
// production code paths.
func Reset() { reg.Reset() }
