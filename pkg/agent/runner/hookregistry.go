package runner

import "github.com/authzed/openagentprimitives/pkg/platform/pipeline"

// HookFactory registers one logical pipeline hook for the runner's data-plane
// registry. Build constructs the hook(s) for a live *Loop at session start and
// returns:
//   - nil/empty when the hook is inactive for this Loop,
//   - one hook for the common case,
//   - many for slice-driven hooks (one content-guard adapter per inspector).
//
// Registration happens at init() (dependency-free); construction is
// dependency-injected from the *Loop. New hooks register via RegisterHook from
// their own package's init() and are blank-imported into internal/cmd/runner.
type HookFactory struct {
	Name  string
	Order int
	Build func(l *Loop) []pipeline.Hook
}

var hookFactories []HookFactory

// RegisterHook adds f to the process-global data-plane factory set. Call from
// init() only.
func RegisterHook(f HookFactory) { hookFactories = append(hookFactories, f) }

// RegisteredHookFactories returns the registered factories (test/diagnostic use).
func RegisteredHookFactories() []HookFactory { return hookFactories }
