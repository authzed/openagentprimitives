// Roster validation. Because rosters are STATIC declarations on AgentClass, the
// whole delegation graph is knowable before anything runs — so a cycle is an
// admission error rather than a runtime loop. That matters: runtime depth
// counters are the industry-standard bound and they demonstrably do not hold.
package agentclass

import (
	"fmt"
	"maps"
	"slices"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ValidateRoster walks the delegation graph rooted at root, refusing cycles and
// any path longer than maxDepth edges. rosters maps an AgentClass name to its
// declared subagents; a name absent from the map is an unknown class.
//
// Pure: no client, no context. The caller gathers the rosters.
func ValidateRoster(root string, rosters map[string][]string, maxDepth int) error {
	// onPath is the current DFS stack (cycle detection); done memoizes fully
	// explored nodes so a diamond is visited once, not exponentially.
	onPath := map[string]bool{}
	done := map[string]bool{}

	var walk func(name string, depth int, path []string) error
	walk = func(name string, depth int, path []string) error {
		if onPath[name] {
			return fmt.Errorf("delegation cycle: %v -> %s", path, name)
		}
		if done[name] {
			return nil
		}
		if depth > maxDepth {
			return fmt.Errorf("delegation path %v exceeds the maximum delegation depth of %d", append(path, name), maxDepth)
		}
		members, ok := rosters[name]
		if !ok {
			return fmt.Errorf("subagent %q is not an AgentClass in this namespace", name)
		}
		onPath[name] = true
		for _, m := range members {
			if err := walk(m, depth+1, append(path, name)); err != nil {
				return err
			}
		}
		onPath[name] = false
		done[name] = true
		return nil
	}
	return walk(root, 0, nil)
}

// subagentsCapabilityCapabilityName is the capability key
// pkg/agent/tool/meta/capability's subagentsCapability registers itself
// under ("subagents") — duplicated here as a literal rather than imported,
// the same wire-shape boundary this package already keeps from
// pkg/agent/tool/meta/capability elsewhere (this package validates the
// SHAPE of spec.capabilities' keys, not their runtime behavior).
const subagentsCapabilityName = "subagents"

// subagentsCapabilityProblem reports (reason, message) for
// AgentClassConditionSubagentsCapabilityGranted, or ("","") when there is
// nothing to warn about: spec.subagents is empty (nothing to delegate to),
// or spec.capabilities already carries a "subagents" entry (whatever its
// {enabled} value — an author who explicitly set it, even to false, made a
// deliberate choice this check has no opinion about; only the OMITTED case
// warns). Pure — safe to call from Reconcile.
func subagentsCapabilityProblem(ac *spiceboxv1alpha1.AgentClass) (reason, message string) {
	if len(ac.Spec.Subagents) == 0 {
		return "", ""
	}
	if _, ok := ac.Spec.Capabilities[subagentsCapabilityName]; ok {
		return "", ""
	}
	return spiceboxv1alpha1.ReasonSubagentsCapabilityMissing, fmt.Sprintf(
		"spec.subagents names %d agent(s) to delegate to, but spec.capabilities has no %q entry — "+
			"without it, the delegate tool is never offered and this class can never actually hand work to anyone on its roster",
		len(ac.Spec.Subagents), subagentsCapabilityName)
}

// validateSubagentModes checks spec.subagentModes against spec.subagents,
// returning a (reason, message) pair to hand setInvalid, or ("", "") when the
// declaration is sound.
//
// Two mistakes are refused rather than ignored, because each produces a class
// whose YAML claims a delegation mode the class does not actually have:
//
//   - a key naming a class that is not on the roster. The entry can never be
//     consulted — PermittedSubagentModes only answers for roster members — so
//     leaving it would read as a granted mode that silently never applies.
//   - a value that is not one of the three modes. Same shape: a misspelled
//     "chatt" widens nothing, and nothing in the manifest says so.
//
// Whether the roster's own members EXIST is not asked here; ValidateRoster
// above already refuses a roster naming a class that does not. This only asks
// whether the modes map lines up with the roster beside it.
func validateSubagentModes(ac *spiceboxv1alpha1.AgentClass) (string, string) {
	if len(ac.Spec.SubagentModes) == 0 {
		return "", ""
	}
	roster := ac.Spec.RosterNames()
	// Sorted rather than ranged over directly: Go randomizes map iteration
	// order, so an unsorted walk would pick a different offender to report on
	// each pass when a class has several, rewriting the status condition every
	// reconcile.
	for _, name := range slices.Sorted(maps.Keys(ac.Spec.SubagentModes)) {
		if !slices.Contains(roster, name) {
			return spiceboxv1alpha1.ReasonRosterInvalid,
				fmt.Sprintf("spec.subagentModes names %q, which is not in spec.subagents; a mode declared for a class that is not on the roster can never take effect", name)
		}
		for _, m := range ac.Spec.SubagentModes[name] {
			if !spiceboxv1alpha1.IsSubagentMode(m) {
				return spiceboxv1alpha1.ReasonRosterInvalid,
					fmt.Sprintf("spec.subagentModes[%q] declares %q, which is not a delegation mode; valid modes are %v",
						name, m, spiceboxv1alpha1.SubagentModesAll())
			}
		}
	}
	return "", ""
}
