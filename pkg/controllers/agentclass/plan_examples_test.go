package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// classWithExample builds a class declaring one authored plan example, over a
// toolkit whose only checked permission is read on `ticket`.
func classWithExample(phases []spiceboxv1alpha1.PlanExamplePhase) (*spiceboxv1alpha1.AgentClass, []spiceboxv1alpha1.SpiceboxToolkit) {
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				PlanGate: &spiceboxv1alpha1.PlanGateConfig{
					Mode:     "enforcing",
					Examples: []spiceboxv1alpha1.PlanExample{{Task: "triage a ticket", Phases: phases}},
				},
			},
		},
	}
	tk := spiceboxv1alpha1.SpiceboxToolkit{
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{{
				Path: []string{"show"},
				Permission: &authz.Permission{
					StateImpact: authz.Readonly,
					Check: &authz.PermissionCheck{
						ResourceType: "ticket", Permission: "read", ResourceIDTemplate: "{id}",
					},
				},
			}},
		},
	}
	return ac, []spiceboxv1alpha1.SpiceboxToolkit{tk}
}

// An example naming a handle the class cannot declare is REFUSED at admission.
//
// The prompt tells agents a handle off the declarable list is silently dropped,
// leaving a narrower ceiling than they think and later calls denied for no
// visible reason. An EXAMPLE carrying such a handle teaches exactly that
// failure into every plan the agent writes — and measured, planners transcribe
// these examples closely, so a wrong one is not inert. Refusing the class names
// the offending handle while an author is still looking at it.
func TestValidatePlanExamples_UnknownHandleIsRefused(t *testing.T) {
	ac, tks := classWithExample([]spiceboxv1alpha1.PlanExamplePhase{{
		ID:          "act",
		Permissions: []string{"perm:read:ticket", "perm:delete:ticket"},
	}})

	reason, msg := validatePlanExamples(ac, nil, tks, nil)

	assert.NotEmpty(t, reason, "an example naming an undeclarable handle must not be admitted")
	assert.Contains(t, msg, "perm:delete:ticket", "the message must name the offending handle")
}

// The good case admits, and says nothing.
func TestValidatePlanExamples_HandlesOnTheSurfaceAreAdmitted(t *testing.T) {
	ac, tks := classWithExample([]spiceboxv1alpha1.PlanExamplePhase{{
		ID:          "look",
		Permissions: []string{"perm:read:ticket"},
		Slots:       []string{"ticket"},
	}})

	reason, msg := validatePlanExamples(ac, nil, tks, nil)

	assert.Empty(t, reason, "msg=%s", msg)
}

// A slot naming a type no check ever keys on is refused for the same reason: a
// copied slot of an unknown type binds nothing.
func TestValidatePlanExamples_UnknownSlotTypeIsRefused(t *testing.T) {
	ac, tks := classWithExample([]spiceboxv1alpha1.PlanExamplePhase{{
		ID: "look", Permissions: []string{"perm:read:ticket"}, Slots: []string{"invoice"},
	}})

	reason, msg := validatePlanExamples(ac, nil, tks, nil)

	assert.NotEmpty(t, reason)
	assert.Contains(t, msg, "invoice")
}

// A malformed handle is refused rather than silently ignored — "read:ticket"
// with no `perm:` prefix is the exact mistake the prompt warns agents about.
func TestValidatePlanExamples_MalformedHandleIsRefused(t *testing.T) {
	ac, tks := classWithExample([]spiceboxv1alpha1.PlanExamplePhase{{
		ID: "look", Permissions: []string{"read:ticket"},
	}})

	reason, msg := validatePlanExamples(ac, nil, tks, nil)

	assert.NotEmpty(t, reason)
	assert.Contains(t, msg, "read:ticket")
}

// No examples, nothing to check. Most classes declare none.
func TestValidatePlanExamples_NoExamplesIsFine(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{}
	reason, _ := validatePlanExamples(ac, nil, nil, nil)
	assert.Empty(t, reason)
}

// A class whose only declarer of a type is a SidecarToolbox can plan with that
// type's handle.
//
// The shipped agent-builder IS this shape: its `workshop_draft` checks live on
// the workshop SidecarToolbox and nowhere else, so a surface built from
// MCPServers and toolkits alone reports `perm:change:workshop_draft` as
// undeclarable and refuses the class over a correct example. A sidecar tool
// carries the identical authz.PermissionCheck an MCPServer tool does —
// SidecarToolboxSpec.Tools IS []MCPServerTool — so which CR kind it arrived in
// was never a reason to judge an example differently.
func TestValidatePlanExamples_ASidecarDeclaredTypeIsDeclarable(t *testing.T) {
	ac, _ := classWithExample([]spiceboxv1alpha1.PlanExamplePhase{{
		ID:          "build",
		Permissions: []string{"perm:change:workshop_draft"},
		Slots:       []string{"workshop_draft"},
	}})
	stb := spiceboxv1alpha1.SidecarToolbox{
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Tools: []spiceboxv1alpha1.MCPServerTool{{
				Name: "apply",
				Permission: &authz.Permission{
					StateImpact: authz.External,
					Check: &authz.PermissionCheck{
						ResourceType: "workshop_draft", Permission: "change", ResourceIDTemplate: "draft",
					},
				},
			}},
		},
	}

	reason, msg := validatePlanExamples(ac, nil, nil, []spiceboxv1alpha1.SidecarToolbox{stb})

	assert.Empty(t, reason, "the sidecar's own check declares the handle and the slot type; msg=%s", msg)
}

// A permission reachable only through an MCP tool's PermissionVariants is
// declarable too. The toolkit branch has always read variants; the MCPServer
// branch did not, so a tool whose base check keys `read` and whose variant keys
// `write` on the same type left `perm:write:…` off the surface — the branch
// least likely to be read closely, which is exactly where the omission sat.
func TestValidatePlanExamples_AnMCPVariantOnlyPermissionIsDeclarable(t *testing.T) {
	ac, _ := classWithExample([]spiceboxv1alpha1.PlanExamplePhase{{
		ID:          "act",
		Permissions: []string{"perm:write:ticket"},
	}})
	srv := spiceboxv1alpha1.MCPServer{
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Tools: []spiceboxv1alpha1.MCPServerTool{{
				Name: "edit_ticket",
				Permission: &authz.Permission{
					StateImpact: authz.Readonly,
					Check: &authz.PermissionCheck{
						ResourceType: "ticket", Permission: "read", ResourceIDTemplate: "{id}",
					},
				},
				PermissionVariants: []authz.PermissionVariant{{
					Check: authz.Permission{
						StateImpact: authz.Readwrite,
						Check: &authz.PermissionCheck{
							ResourceType: "ticket", Permission: "write", ResourceIDTemplate: "{id}",
						},
					},
				}},
			}},
		},
	}

	reason, msg := validatePlanExamples(ac, []spiceboxv1alpha1.MCPServer{srv}, nil, nil)

	assert.Empty(t, reason, "a variant's own check declares its permission too; msg=%s", msg)
}
