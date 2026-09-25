package agentclass

import (
	"fmt"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// validatePlanExamples refuses an authored plan example that names a handle or
// slot type this class cannot actually declare.
//
// The prompt tells agents that a handle off the declarable list is silently
// DROPPED from their phase, leaving a narrower ceiling than they think and
// every later call denied for no visible reason. An example carrying such a
// handle teaches precisely that, and planners were measured transcribing these
// examples closely — labels, justifications and all — so a wrong example is not
// inert decoration. Refusing names the offender while its author is still
// looking at it, instead of surfacing weeks later as an inexplicable denial.
//
// Checked against the same walk the surface is built from: a permission is
// declarable only if some TOOL checks it. A schema may declare permissions no
// tool ever keys on, and those are not plannable.
func validatePlanExamples(
	ac *spiceboxv1alpha1.AgentClass,
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
) (string, string) {
	examples := planExamplesOf(ac)
	if len(examples) == 0 {
		return "", ""
	}
	handles, slotTypes := declarableSurface(mcpServers, toolkits, sidecarToolboxes)

	for _, ex := range examples {
		for _, ph := range ex.Phases {
			for _, h := range ph.Permissions {
				if !strings.HasPrefix(h, "perm:") && !strings.HasPrefix(h, "tool:") {
					return spiceboxv1alpha1.ReasonPlanExampleInvalid, fmt.Sprintf(
						"planGate.examples[%q].phases[%q]: %q is not a handle — write perm:<permission>:<resourceType> or tool:<name>",
						ex.Task, ph.ID, h)
				}
				if strings.HasPrefix(h, "perm:") && !has(handles, h) {
					return spiceboxv1alpha1.ReasonPlanExampleInvalid, fmt.Sprintf(
						"planGate.examples[%q].phases[%q]: %q is not declarable on this class; declarable: %s",
						ex.Task, ph.ID, h, sortedKeys(handles))
				}
			}
			for _, s := range ph.Slots {
				if !has(slotTypes, s) {
					return spiceboxv1alpha1.ReasonPlanExampleInvalid, fmt.Sprintf(
						"planGate.examples[%q].phases[%q]: slot type %q is not keyed by any tool on this class; known: %s",
						ex.Task, ph.ID, s, sortedKeys(slotTypes))
				}
			}
		}
	}
	return "", ""
}

// planExamplesOf reads the authored examples, tolerating every absent level.
func planExamplesOf(ac *spiceboxv1alpha1.AgentClass) []spiceboxv1alpha1.PlanExample {
	if ac == nil || ac.Spec.Authz == nil || ac.Spec.Authz.PlanGate == nil {
		return nil
	}
	return ac.Spec.Authz.PlanGate.Examples
}

// declarableSurface returns the handles and slot types this class's TOOLS
// actually key on — the same source permsurface.Enumerate reads at runtime,
// walked here from the CRD specs because the controller has no live tools.
//
// Built from keyingSources (slot_valuekey.go) rather than from a walk of its
// own: that function already flattens all three tool-bearing CR kinds, and
// already reads each declarer's PermissionVariants beside its base check —
// which is exactly the set permsurface.Enumerate sees at runtime. A second,
// independent walk of the same kinds is how this one came to miss both halves.
// A SidecarToolbox's tools were absent, and the shipped agent-builder is that
// shape: its sidecar is the ONLY declarer of workshop_draft, so an example
// naming perm:change:workshop_draft would have refused a correct class. An
// MCPServer tool's variants were absent too, so a permission reachable only
// through a variant read as off-surface while the toolkit branch beside it
// read its variants fine.
func declarableSurface(
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
) (handles, slotTypes map[string]struct{}) {
	handles, slotTypes = map[string]struct{}{}, map[string]struct{}{}
	for _, src := range keyingSources(mcpServers, toolkits, sidecarToolboxes) {
		for _, check := range src.checks {
			if check.ResourceType == "" || check.Permission == "" {
				continue
			}
			handles["perm:"+check.Permission+":"+check.ResourceType] = struct{}{}
			slotTypes[check.ResourceType] = struct{}{}
		}
	}
	return handles, slotTypes
}

func has(m map[string]struct{}, k string) bool {
	_, ok := m[k]
	return ok
}
