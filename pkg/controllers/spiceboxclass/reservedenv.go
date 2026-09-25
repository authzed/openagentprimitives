package spiceboxclass

import (
	"sort"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
)

// sensitiveEnvNames returns the set of env-var names declared `sensitive`
// by any builtin toolkit. These are auth-injected via AgentIdentity
// bindings, so a SpiceboxClass must not shadow them through EnvDefaults.
// User-defined SpiceboxToolkit CRs are intentionally NOT scanned here:
// their env.allowed is author-controlled, and watching every
// SpiceboxToolkit to re-validate classes would add reconcile churn
// disproportionate to the threat. Builtin provider toolkits (gh,
// claude, …) are the realistic credential surface.
func sensitiveEnvNames(reg *registry.Registry) []string {
	if reg == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, tk := range reg.Builtins() {
		for _, ev := range tk.Env.Allowed {
			if ev.Sensitive {
				if _, dup := seen[ev.Name]; !dup {
					seen[ev.Name] = struct{}{}
					out = append(out, ev.Name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}
