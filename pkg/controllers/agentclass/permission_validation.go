package agentclass

import (
	"context"
	"fmt"
	"net/url"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// validateSiteURL ensures SiteURL, when set, is a parseable http or
// https URL. Empty string is allowed (the field is +optional).
func validateSiteURL(siteURL string) error {
	if siteURL == "" {
		return nil
	}
	u, err := url.Parse(siteURL)
	if err != nil {
		return fmt.Errorf("siteURL %q: %w", siteURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("siteURL %q: scheme must be http or https (got %q)", siteURL, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("siteURL %q: missing host", siteURL)
	}
	return nil
}

// validatePermissions walks every tool referenced by the AgentClass
// and enforces the slice-1 rules. Returns (reason, message); empty
// reason means valid.
//
// When the AgentClass declares ToolAuthMode=="enforcing" (the slice-2
// strictness default), the walk additionally validates that every
// state-mutating toolkit subcommand (destructive or writes effect)
// declares its OWN Permission block — no inheritance from the
// toolkit-level default. In "permissive" or "disabled" mode the
// toolkit-subcommand walk is skipped so existing toolkit YAMLs that
// haven't been backfilled with per-subcommand permission specs don't
// block the AgentClass from validating.
//
// sidecarToolboxes is carried for the two walks that ask what tools this class
// HAS: the plan-example check (their tools are part of the declarable surface
// an example is judged against) and the trifecta ceiling (their tools are part
// of what "this class can act" means), exactly as an MCPServer's are in both.
// The per-tool shape rules below deliberately do NOT walk them — that gate is
// the runner's (see the deploy declaration's header), and moving it here is a
// separate decision, not a side effect of this one.
func validatePermissions(
	ac *spiceboxv1alpha1.AgentClass,
	mcpServers []spiceboxv1alpha1.MCPServer,
	toolkits []spiceboxv1alpha1.SpiceboxToolkit,
	sidecarToolboxes []spiceboxv1alpha1.SidecarToolbox,
) (string, string) {
	// Slot validation is NOT here: it is unconditional and runs earlier in
	// Reconcile. This function only fires when the class references an
	// MCPServer or a toolkit, which is the wrong gate for a check about the
	// class's own declarations.
	if reason, msg := validatePlanGateMode(ac); reason != "" {
		return reason, msg
	}
	// Authored plan examples are checked HERE rather than beside the slot
	// declarations above, because unlike those they are not an internal
	// consistency claim: an example is right or wrong only against the tools
	// this class actually has, and those are resolved by the time this runs.
	if reason, msg := validatePlanExamples(ac, mcpServers, toolkits, sidecarToolboxes); reason != "" {
		return reason, msg
	}
	// The trifecta ceiling is checked here for the same reason as the examples
	// above: "this class never acts" is a claim about the tools it actually
	// has, not an internal consistency claim, so it can only be judged once
	// those are resolved.
	if reason, msg := validateTrifectaCeiling(ac, mcpServers, toolkits, sidecarToolboxes); reason != "" {
		return reason, msg
	}
	for _, srv := range mcpServers {
		for _, tool := range srv.Spec.Tools {
			ref := fmt.Sprintf("MCPServer/%s tool/%s", srv.Name, tool.Name)
			if tool.Permission == nil {
				return spiceboxv1alpha1.ReasonToolPermissionMissing,
					fmt.Sprintf("%s: no permission spec; declare stateImpact: stateless or passthrough to opt out", ref)
			}
			if reason, msg := validatePermissionShape(*tool.Permission, ref); reason != "" {
				return reason, msg
			}
		}
	}
	// Shape validation runs on toolkit checks in EVERY mode, exactly as it does
	// for MCP tools above. A check that mints an object id from a free-form
	// value with a lossy transform is an authorization defect in the spec, not a
	// property of the mode the class happens to run in — and permissive mode
	// still records the decisions a human later reads.
	//
	// This is parity that was missing rather than a new rule. validateToolkitSubcommands
	// checked only that a state-mutating subcommand HAS a permission and that its
	// stateImpact is sane; nothing ran the transform rules over its check. So a
	// toolkit could ship an unknown transform, an empty resourceType, or a
	// non-injective terminal on an expr-keyed check and be admitted — the last of
	// which is what let `spicedb_object_id` sit under a resourceIDExpr in the
	// shipped git and gh toolkits, where ".com" and ",com" mint one id.
	for _, tk := range toolkits {
		for _, sub := range tk.Spec.Subcommands {
			ref := fmt.Sprintf("SpiceboxToolkit/%s subcommand/%s", tk.Name, subcommandPath(sub.Path))
			if sub.Permission != nil {
				if reason, msg := validatePermissionShape(*sub.Permission, ref); reason != "" {
					return reason, msg
				}
			}
			// Variants are checked too: a variant can key a resource type
			// differently from its base, and it is the branch least likely to be
			// read closely.
			for i, v := range sub.PermissionVariants {
				if reason, msg := validatePermissionShape(v.Check, fmt.Sprintf("%s variant[%d]", ref, i)); reason != "" {
					return reason, msg
				}
			}
		}
	}
	if effectiveToolAuthMode(ac) == toolAuthModeEnforcing {
		for _, tk := range toolkits {
			if reason, msg := validateToolkitSubcommands(tk); reason != "" {
				return reason, msg
			}
		}
	}
	return "", ""
}

// toolAuthMode constants for the validator. Mirrors the runner-side
// constants in pkg/agent/runner — kept local so this package stays off
// the runner's import graph.
const (
	toolAuthModeEnforcing  = "enforcing"
	toolAuthModePermissive = "permissive"
	toolAuthModeDisabled   = "disabled"
)

// effectiveToolAuthMode returns the AgentClass's mode, defaulting to
// enforcing on empty (matches the CRD default).
func effectiveToolAuthMode(ac *spiceboxv1alpha1.AgentClass) string {
	mode := ac.Spec.GetAuthz().GetToolCalls().Mode
	switch mode {
	case toolAuthModePermissive, toolAuthModeEnforcing, toolAuthModeDisabled:
		return mode
	}
	return toolAuthModeEnforcing
}

// validateToolkitSubcommands enforces the slice-2 enforcing-mode rule:
// every subcommand whose Effects flag it as state-mutating (Destructive
// true OR Writes non-empty) MUST declare its own Permission block. The
// toolkit-level default does NOT cover state-mutating subcommands —
// inheriting "stateless" or a vague readonly default onto a destructive
// op would silently bypass the runner's authz gate.
//
// Non-mutating subcommands (read-only listing, etc.) inherit the
// toolkit-level Permission if present; if BOTH the toolkit-level and
// per-subcommand permissions are absent the validator still tolerates
// it under the assumption the subcommand is benign — the runner
// defaults to a no-op permission for tools without a spec.
func validateToolkitSubcommands(tk spiceboxv1alpha1.SpiceboxToolkit) (string, string) {
	for _, sub := range tk.Spec.Subcommands {
		if !isStateMutating(sub.Effects) {
			continue
		}
		ref := fmt.Sprintf("SpiceboxToolkit/%s subcommand/%s", tk.Name, subcommandPath(sub.Path))
		if sub.Permission == nil {
			return spiceboxv1alpha1.ReasonToolPermissionMissing,
				fmt.Sprintf("%s: subcommand has destructive effects OR writes but no permission block; declare a non-passthrough stateImpact (readonly/readwrite/external) explicitly — enforcing mode does NOT inherit the toolkit-level default for state-mutating subcommands", ref)
		}
		// State-mutating subcommands must declare a non-passthrough
		// state impact. Stateless / passthrough on a destructive
		// subcommand is almost certainly a misconfiguration.
		switch sub.Permission.StateImpact {
		case authz.Stateless, authz.Passthrough:
			return spiceboxv1alpha1.ReasonPermissionSpecInvalid,
				fmt.Sprintf("%s: state-mutating subcommand (destructive or writes effect) cannot use stateImpact=%s; use readwrite or external", ref, sub.Permission.StateImpact)
		}
		if reason, msg := validatePermissionShape(*sub.Permission, ref); reason != "" {
			return reason, msg
		}
	}
	return "", ""
}

// isStateMutating returns true when a subcommand's Effects mark it as
// destructive or as having any writes target.
func isStateMutating(e spiceboxv1alpha1.ToolkitEffects) bool {
	if e.Destructive {
		return true
	}
	if len(e.Writes) > 0 {
		return true
	}
	return false
}

func subcommandPath(path []string) string {
	if len(path) == 0 {
		return "(empty)"
	}
	out := path[0]
	for _, p := range path[1:] {
		out += " " + p
	}
	return out
}

func validatePermissionShape(p authz.Permission, ref string) (string, string) {
	switch p.StateImpact {
	case authz.Stateless, authz.Passthrough:
		if p.Check != nil {
			return spiceboxv1alpha1.ReasonPermissionSpecInvalid,
				fmt.Sprintf("%s: stateImpact=%s must NOT set check", ref, p.StateImpact)
		}
		return "", ""
	case authz.Readonly, authz.Readwrite, authz.External:
		if p.Check == nil {
			return spiceboxv1alpha1.ReasonPermissionSpecInvalid,
				fmt.Sprintf("%s: stateImpact=%s requires a check block", ref, p.StateImpact)
		}
	default:
		return spiceboxv1alpha1.ReasonPermissionSpecInvalid,
			fmt.Sprintf("%s: unknown stateImpact %q", ref, p.StateImpact)
	}

	if p.Check.ResourceType == "" {
		return spiceboxv1alpha1.ReasonPermissionSpecInvalid,
			fmt.Sprintf("%s: check.resourceType is empty", ref)
	}
	if p.Check.Permission == "" {
		return spiceboxv1alpha1.ReasonPermissionSpecInvalid,
			fmt.Sprintf("%s: check.permission is empty", ref)
	}
	for _, name := range p.Check.ResourceIDTransforms {
		if !authz.IsRegisteredTransform(name) {
			return spiceboxv1alpha1.ReasonPermissionTransformUnknown,
				fmt.Sprintf("%s: unknown transform %q", ref, name)
		}
	}

	// A resourceIDExpr mints an object id from a FREE-FORM value — a URL, a
	// path, an address the agent supplies. There the transform chain decides
	// which values are the same resource, so a non-injective transform is an
	// authorization defect and not a formatting choice: basename makes
	// /etc/passwd and /home/u/passwd one object, and spicedb_object_id maps
	// every illegal rune to "-" and dedupes runs. Either way a human approves
	// one target and silently authorizes another.
	//
	// The template branch is left alone deliberately. There the id comes from a
	// named argument that already IS a distinct resource, and tidying it is the
	// normal use of these transforms.
	if p.Check.ResourceIDExpr != "" {
		// UnsafeSlotTransforms, not NonInjectiveTransforms: injectivity is a PROXY
		// for "the value the human approved is the only one reaching the grant",
		// and a transform that folds only ALIASES of one resource satisfies that
		// without being injective. casefold_identity on a case-insensitive
		// namespace merges two spellings of one GitHub repository, never two
		// repositories -- see its registry entry for the obligation that carries.
		if bad := authz.UnsafeSlotTransforms(p.Check.ResourceIDTransforms); len(bad) > 0 {
			return spiceboxv1alpha1.ReasonPermissionSpecInvalid,
				fmt.Sprintf("%s: check.resourceIDExpr derives an object id from a free-form value, "+
					"so every transform must preserve resource identity; %v may map two different "+
					"targets onto one id, which would let an approval for one authorize the other. "+
					"Use sha256 (optionally after normalize_url), or casefold_identity where case "+
					"does not distinguish resources.", ref, bad)
		}
	}
	return "", ""
}

// listMCPServersFor returns every MCPServer referenced by the
// AgentClass. Resolved from the cluster.
func listMCPServersFor(ctx context.Context, c client.Reader, ac *spiceboxv1alpha1.AgentClass) ([]spiceboxv1alpha1.MCPServer, error) {
	out := make([]spiceboxv1alpha1.MCPServer, 0, len(ac.Spec.MCPServers))
	for _, ref := range ac.Spec.MCPServers {
		var srv spiceboxv1alpha1.MCPServer
		if err := c.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: ref.Ref}, &srv); err != nil {
			return nil, fmt.Errorf("get MCPServer/%s: %w", ref.Ref, err)
		}
		out = append(out, srv)
	}
	return out, nil
}

// listToolkitsFor returns every SpiceboxToolkit reachable from the
// AgentClass via its toolBundles → toolspecs → toolkit references.
// Toolkits that resolve from the embedded registry (not present as
// cluster CRs) contribute nothing to the walk — the controller only
// validates what an operator has authored / has visibility into via
// kubectl. Missing-toolkit failures are reported by the
// existing slice-1 toolspec-validity check, not here.
func listToolkitsFor(ctx context.Context, c client.Reader, ac *spiceboxv1alpha1.AgentClass) ([]spiceboxv1alpha1.SpiceboxToolkit, error) {
	if len(ac.Spec.ToolBundles) == 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	var out []spiceboxv1alpha1.SpiceboxToolkit
	for _, b := range ac.Spec.ToolBundles {
		for _, tsName := range b.Toolspecs {
			var ts spiceboxv1alpha1.SpiceboxToolspec
			if err := c.Get(ctx, types.NamespacedName{Name: tsName}, &ts); err != nil {
				// Slice-1 validation already failed earlier in
				// Reconcile if the toolspec is missing; we shouldn't
				// be here. Tolerate it anyway to avoid coupling the
				// two error paths.
				continue
			}
			tkName := ts.Spec.Toolkit.Name
			if tkName == "" {
				continue
			}
			if _, dup := seen[tkName]; dup {
				continue
			}
			var tk spiceboxv1alpha1.SpiceboxToolkit
			if err := c.Get(ctx, types.NamespacedName{Name: tkName}, &tk); err != nil {
				// Missing toolkit (embedded registry-only) — skip;
				// no subcommand walk possible.
				continue
			}
			seen[tkName] = struct{}{}
			out = append(out, tk)
		}
	}
	return out, nil
}

// validatePlanGateMode rejects planGate.mode=enforcing unless tool-call authz
// is also enforcing.
//
// The plan gate enforces the CLASS axis on its own (ceiling membership, no
// SpiceDB Check). The INSTANCE axis — which specific resource a call may touch
// — rides entirely on tool_call_authz. So with planGate enforcing and
// toolCalls permissive or disabled, per-resource gating silently stops while
// the class axis keeps refusing calls, which looks from the outside like the
// gate is working correctly.
//
// A validation error rather than a silent narrowing: the operator asked for
// enforcement and would otherwise get half of it without being told.
func validatePlanGateMode(ac *spiceboxv1alpha1.AgentClass) (string, string) {
	pg := ac.Spec.GetAuthz().PlanGate
	if pg == nil || pg.Mode != "enforcing" {
		return "", ""
	}
	if effectiveToolAuthMode(ac) == toolAuthModeEnforcing {
		return "", ""
	}
	return spiceboxv1alpha1.ReasonPlanGateRequiresToolCallEnforcing,
		fmt.Sprintf("authz.planGate.mode is %q but authz.toolCalls.mode is %q: the plan gate enforces "+
			"which KINDS of action are allowed, while WHICH resource each call may touch is enforced by "+
			"tool-call authz. Set authz.toolCalls.mode: enforcing, or lower planGate.mode to logging.",
			pg.Mode, effectiveToolAuthMode(ac))
}

// validateSlotsMigration rejects an AgentClass still using the pre-rename
// spec.boundEntities spelling.
//
// A rename of an AUTHZ field cannot be silent. If the old spelling were simply
// ignored, an upgraded class would keep running with its instance constraints
// quietly gone — nothing fails, the agent is just less bounded than its author
// wrote, and the drift is invisible until something is touched that should not
// have been.
//
// So the field still deserializes and its presence is a hard error naming the
// replacement. That trades an upgrade break for an undetectable authz
// regression, which is the right way round.
func validateSlotsMigration(ac *spiceboxv1alpha1.AgentClass) (string, string) {
	if len(ac.Spec.BoundEntities) == 0 {
		return "", ""
	}
	return spiceboxv1alpha1.ReasonBoundEntitiesRenamed,
		fmt.Sprintf("spec.boundEntities is no longer read (%d declared); it was renamed to "+
			"spec.authz.slots. Move the list verbatim — the fields are unchanged — so this "+
			"agent's instance constraints keep applying. It is rejected rather than ignored "+
			"because silently dropping them would leave the agent less constrained than written.",
			len(ac.Spec.BoundEntities))
}
