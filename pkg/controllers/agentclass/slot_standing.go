package agentclass

import (
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// resolveStandingFor decides how approval for a resource type is governed:
// whether SpiceDB says who may approve (StandingRequired, plus the permission
// they must hold), or nothing local does and the session's approvers decide
// (StandingSessionOnly).
//
// Returns an ERROR when no reachable fragment declares a standing for the type.
// There is no default and there deliberately cannot be one: guessing
// session-only silently widens who may approve a type nobody classified, and
// guessing required makes a forge-governed type permanently unbindable. Both
// are silent, so an undeclared type is refused and the AgentClass says why.
//
// STRICTEST fragment wins when several declare the same type. Composition is
// global, so two toolkits can contribute the same definition, and resolving to
// the laxer answer would let adding an unrelated toolkit quietly weaken a type
// somebody deliberately marked required. The loop never breaks early on a
// required match, so this holds regardless of declaration order.
func resolveStandingFor(
	resourceType string,
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
	veto map[string]struct{},
) (standing, approverPermission string, err error) {
	var declared bool
	for _, res := range standingSources(mcpServers, toolkits, sidecarToolboxes) {
		if res.Name != resourceType {
			continue
		}
		if verr := res.ValidateStanding(); verr != nil {
			return "", "", verr
		}
		declared = true
		if res.Standing == spiceboxv1alpha1.StandingRequired {
			standing = spiceboxv1alpha1.StandingRequired
			approverPermission = res.ApproverPermission
		} else if standing == "" {
			standing = spiceboxv1alpha1.StandingSessionOnly
		}
	}
	if !declared {
		return "", "", fmt.Errorf(
			"resource type %q declares no standing; every type must state whether SpiceDB governs who may approve it (standing: %q with an approverPermission) or nothing local does (standing: %q). There is no default",
			resourceType, spiceboxv1alpha1.StandingRequired, spiceboxv1alpha1.StandingSessionOnly)
	}

	// The admin veto is a CEILING no fragment may widen past, so it is applied
	// after the fragments rather than before: it can only tighten the answer.
	//
	// It can also be unsatisfiable. Forcing `required` on a type that names no
	// approverPermission leaves the router with no pool to resolve, which would
	// dead-end every approval for it. That is refused out loud rather than
	// written to status, because a silently unapprovable type is the exact
	// failure this whole change exists to remove.
	if _, vetoed := veto[resourceType]; vetoed {
		if approverPermission == "" {
			return "", "", fmt.Errorf(
				"resource type %q is forced to standing %q by requireStandingFor, but declares no approverPermission, so no one could ever be resolved to approve it; the type's schema must name the permission an approver holds",
				resourceType, spiceboxv1alpha1.StandingRequired)
		}
		return spiceboxv1alpha1.StandingRequired, approverPermission, nil
	}
	return standing, approverPermission, nil
}

// standingSources flattens every schema fragment reachable from this class.
// Mirrors keyingSources in slot_valuekey.go, which walks the same three CR
// kinds for the value→id transform chains — kept as a separate function
// because that one walks TOOLS (checks) and this one walks SCHEMA RESOURCES,
// a different part of the same CRs.
func standingSources(
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
) []spiceboxv1alpha1.SpiceDBResource {
	var out []spiceboxv1alpha1.SpiceDBResource
	for _, srv := range mcpServers {
		if srv.Spec.SpiceDBSchema != nil {
			out = append(out, srv.Spec.SpiceDBSchema.Resources...)
		}
	}
	for _, tk := range toolkits {
		if tk.Spec.SpiceDBSchema != nil {
			out = append(out, tk.Spec.SpiceDBSchema.Resources...)
		}
	}
	// SidecarToolbox fragments are governed exactly like MCPServer ones: a
	// sidecar tool's read/egress data is tainted by the resource type its
	// toolResourceMap names, and an approval on that leak needs the type's
	// standing. Omitting sidecars here made every sidecar-declared type read as
	// "no standing", which fail-closes the leak approval before it can prompt.
	for _, sc := range sidecarToolboxes {
		if sc.Spec.SpiceDBSchema != nil {
			out = append(out, sc.Spec.SpiceDBSchema.Resources...)
		}
	}
	return out
}
