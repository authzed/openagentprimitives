// Package apnative registers the built-in agent harness: the runner binary
// running runner.Loop. It is the default when AgentClass.spec.harness is
// absent.
//
// Its ContainerSpec is deliberately EMPTY. The runner image's ENTRYPOINT is
// the runner binary, and every variable it needs is platform wiring that
// podspec already computes for all harnesses. An empty contribution is what
// makes introducing the seam behavior-preserving: the runner container built
// through the harness path is byte-identical to the one built before it.
package apnative

import (
	"github.com/authzed/openagentprimitives/pkg/agent/harness"
	"github.com/authzed/openagentprimitives/pkg/agent/harness/registry"
)

func init() { registry.Register(Harness{}) }

// Harness is the built-in loop. Stateless.
type Harness struct{}

func (Harness) Name() string { return harness.DefaultName }

// ModelAccess is Proxied: the runner reaches its provider through
// pkg/agent/llm, so AP already observes every model call and can enforce
// cost, pinning, and the token budget without a proxy hop.
func (Harness) ModelAccess() harness.ModelAccessMode { return harness.Proxied }

// Container contributes nothing — see the package doc.
func (Harness) Container(harness.HarnessOpts) (harness.ContainerSpec, error) {
	return harness.ContainerSpec{}, nil
}
