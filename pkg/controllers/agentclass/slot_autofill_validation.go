package agentclass

import (
	"context"
	"fmt"
	"path/filepath"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// validateSlotAutofill refuses a slot whose autoFillArgs target a SANDBOX tool,
// because auto-fill cannot reach one and fails silently when it tries.
//
// authz.fillFromGrants sets args[ArgName] on the TOP-LEVEL args object. That is
// the right shape for an MCP tool, whose LLM-facing arguments ARE named. A
// sandbox tool's are not: it receives {operation_id, _reason, args: [argv]}, so
// the fill lands beside an untouched argv and the tool runs the command the
// agent wrote. The check does not see it either — a sandbox tool's named args
// come from the toolkit parser's view of argv (tool.NamedCallArgs), not from the
// JSON object — so the value is inert on both paths and nothing errors.
//
// There is a second reason this cannot simply be made to work here: the value
// available to fill is the SpiceDB OBJECT ID, already through the check's
// resourceIDTransforms. `https=3A//github=2Ecom/demo-org/demo-repo` is not a
// git remote, and a SlotBinding carries nothing else. The terminal escape is
// invertible, but normalize_url ahead of it is not — it folds scheme and host
// case, default ports and dot segments on purpose, so the spelling the agent
// would have to pass back is gone. Rendering argv from a binding needs the
// pre-transform value carried alongside it, which is a change to the binding,
// not to this function.
//
// So the honest answer today is to refuse the combination rather than accept an
// opt-in that binds nothing. The instance axis itself is unaffected and works
// for sandbox tools through grants and checks.
func validateSlotAutofill(ac *spiceboxv1alpha1.AgentClass, sandboxToolNames []string) (reason, msg string) {
	if ac == nil || len(sandboxToolNames) == 0 {
		return "", ""
	}
	for _, slot := range ac.Spec.GetSlots() {
		for _, af := range slot.AutoFillArgs {
			for _, toolName := range sandboxToolNames {
				if !autofillPatternMatches(af.ToolNamePattern, toolName) {
					continue
				}
				why := fmt.Sprintf("toolNamePattern %q matches it", af.ToolNamePattern)
				if af.ToolNamePattern == "" {
					why = "an omitted toolNamePattern matches every tool"
				}
				return spiceboxv1alpha1.ReasonAgentClassSlotAutofillUnfillable, fmt.Sprintf(
					"authz.slots[%s].autoFillArgs cannot fill arg %q on sandbox tool %q (%s): "+
						"a sandbox tool takes argv, not named arguments, so the filled value would "+
						"be ignored by both the tool and its permission check. Narrow the "+
						"toolNamePattern to the MCP tools that take %q, or drop autoFillArgs — "+
						"the slot still authorizes %s per instance without it",
					slot.ResourceType, af.ArgName, toolName, why, af.ArgName, slot.ResourceType)
			}
		}
	}
	return "", ""
}

// autofillPatternMatches mirrors the matching authz.fillFromGrants performs, so
// what validation refuses is exactly what would have been filled.
//
// An EMPTY pattern matches every tool — that is fillFromGrants' own rule, and it
// is why an omitted pattern is a refusal rather than a pass. A malformed pattern
// matches nothing: filepath.Match returns an error, and treating that as a match
// would refuse a class over a typo the runtime would simply skip.
func autofillPatternMatches(pattern, toolName string) bool {
	if pattern == "" {
		return true
	}
	ok, err := filepath.Match(pattern, toolName)
	return err == nil && ok
}

// sandboxToolNamesFor returns the LLM-facing names of every sandbox tool this
// class synthesizes — "<bundleName>_<classToolName>", matching
// sandbox.SandboxTool.Name().
//
// A missing SpiceboxClass is skipped rather than an error: slice-1 validation
// has already failed the class for that, and duplicating the failure here would
// couple two unrelated error paths. The consequence is a narrower name list,
// which can only make this validator more permissive — never wrongly refuse.
func sandboxToolNamesFor(ctx context.Context, c client.Reader, ac *spiceboxv1alpha1.AgentClass) ([]string, error) {
	if ac == nil || !slotsDeclareAutofill(ac) {
		// The list is used for one thing. Skip the Gets entirely when no slot
		// asks to fill anything, which is the overwhelmingly common case.
		return nil, nil
	}
	var out []string
	for _, b := range ac.Spec.ToolBundles {
		if b.Class == "" {
			continue
		}
		var sc spiceboxv1alpha1.SpiceboxClass
		if err := c.Get(ctx, types.NamespacedName{Namespace: ac.Namespace, Name: b.Class}, &sc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get SpiceboxClass %s/%s: %w", ac.Namespace, b.Class, err)
		}
		for _, tl := range sc.Spec.Tools {
			out = append(out, b.Name+"_"+tl.Name)
		}
	}
	return out, nil
}

// slotsDeclareAutofill reports whether any slot asks to fill a tool argument.
func slotsDeclareAutofill(ac *spiceboxv1alpha1.AgentClass) bool {
	for _, slot := range ac.Spec.GetSlots() {
		if len(slot.AutoFillArgs) > 0 {
			return true
		}
	}
	return false
}
