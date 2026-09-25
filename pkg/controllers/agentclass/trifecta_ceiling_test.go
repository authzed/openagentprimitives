package agentclass

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// classDeclaringNeverConsequential builds a class with the ceiling set.
func classDeclaringNeverConsequential(t *testing.T, declared bool) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-reader", Namespace: "ns"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				Trifecta: &spiceboxv1alpha1.TrifectaConfig{NeverConsequential: declared},
			},
		},
	}
}

// serverWithTool builds an MCPServer whose single tool carries impact.
func serverWithTool(t *testing.T, toolName, permission, resourceType string, impact authz.StateImpact) spiceboxv1alpha1.MCPServer {
	t.Helper()
	return spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-srv", Namespace: "ns"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Tools: []spiceboxv1alpha1.MCPServerTool{{
				Name: toolName,
				Permission: &authz.Permission{
					StateImpact: impact,
					Check:       &authz.PermissionCheck{Permission: permission, ResourceType: resourceType},
				},
			}},
		},
	}
}

// toolboxWithTools builds a SidecarToolbox whose tools carry the given
// permissions. SidecarToolboxSpec.Tools IS []MCPServerTool, so this is the same
// shape serverWithTool builds — which is the point: nothing about the CR kind
// changes what a tool's stateImpact means.
func toolboxWithTools(t *testing.T, name string, tools ...spiceboxv1alpha1.MCPServerTool) spiceboxv1alpha1.SidecarToolbox {
	t.Helper()
	return spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       spiceboxv1alpha1.SidecarToolboxSpec{Tools: tools},
	}
}

// checkedTool is a tool whose permission keys a resource type. No *testing.T:
// it builds a struct and has no failure point to attribute.
func checkedTool(toolName, permission, resourceType string, impact authz.StateImpact) spiceboxv1alpha1.MCPServerTool {
	return spiceboxv1alpha1.MCPServerTool{
		Name: toolName,
		Permission: &authz.Permission{
			StateImpact: impact,
			Check:       &authz.PermissionCheck{Permission: permission, ResourceType: resourceType},
		},
	}
}

// classHoldingToolbox references a SidecarToolbox by name and nothing else —
// the shape of the shipped builder, and the shape leg C was blind to.
func classHoldingToolbox(t *testing.T, toolboxName string) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-builder", Namespace: "ns"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SidecarToolboxes: []spiceboxv1alpha1.AgentClassSidecarToolboxRef{
				{Name: "demo", Ref: toolboxName},
			},
		},
	}
}

// TestCeilingContradictedByAWritingTool is the whole point of the declaration.
//
// A class cannot both promise it never acts and be given the means to. Caught
// at APPLY time against the derived surface rather than surfacing later as a
// delegation refusal nobody can trace back to the YAML.
func TestCeilingContradictedByAWritingTool(t *testing.T) {
	ac := classDeclaringNeverConsequential(t, true)
	srv := serverWithTool(t, "write_issue", "write", "linear_issue", authz.Readwrite)

	reason, msg := validateTrifectaCeiling(ac, []spiceboxv1alpha1.MCPServer{srv}, nil, nil)

	assert.Equal(t, spiceboxv1alpha1.ReasonTrifectaCeilingContradicted, reason)
	assert.Contains(t, msg, "perm:write:linear_issue",
		"the offending handle must be named: an operator has to know WHICH permission to remove")
}

// TestCeilingIsSatisfiedByReadOnlyTools pins that readonly is not leg C.
//
// This is the case the ceiling exists to serve — a class that can look at
// things and nothing more. Reporting readonly here would make the declaration
// unusable for exactly the classes it suits best.
func TestCeilingIsSatisfiedByReadOnlyTools(t *testing.T) {
	ac := classDeclaringNeverConsequential(t, true)
	srv := serverWithTool(t, "read_issue", "view", "linear_issue", authz.Readonly)

	reason, msg := validateTrifectaCeiling(ac, []spiceboxv1alpha1.MCPServer{srv}, nil, nil)

	assert.Empty(t, reason, "a read-only class satisfies a never-act ceiling; got %q", msg)
}

// TestExternalAlsoContradicts: external is the most consequential tier, so it
// must be caught alongside readwrite rather than only the middle one.
func TestExternalAlsoContradicts(t *testing.T) {
	ac := classDeclaringNeverConsequential(t, true)
	srv := serverWithTool(t, "open_pr", "push", "github_repo", authz.External)

	reason, _ := validateTrifectaCeiling(ac, []spiceboxv1alpha1.MCPServer{srv}, nil, nil)
	assert.Equal(t, spiceboxv1alpha1.ReasonTrifectaCeilingContradicted, reason)
}

// TestNoCeilingDeclaredIsNeverValidated keeps this inert for every class that
// did not ask. A class with writing tools and no declaration is ordinary.
func TestNoCeilingDeclaredIsNeverValidated(t *testing.T) {
	srv := serverWithTool(t, "write_issue", "write", "linear_issue", authz.Readwrite)

	reason, _ := validateTrifectaCeiling(classDeclaringNeverConsequential(t, false),
		[]spiceboxv1alpha1.MCPServer{srv}, nil, nil)
	assert.Empty(t, reason, "a class that declared nothing must not be judged against a ceiling")

	bare := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "ns"}}
	reason, _ = validateTrifectaCeiling(bare, []spiceboxv1alpha1.MCPServer{srv}, nil, nil)
	assert.Empty(t, reason, "no authz block at all must not panic or judge")
}

// TestOffendersAreStableAcrossCalls pins the sort.
//
// The list lands in a status condition message. An unstable order would
// rewrite status on every reconcile and churn field ownership — the SSA
// idempotency rule this repo already learned the hard way.
func TestOffendersAreStableAcrossCalls(t *testing.T) {
	servers := []spiceboxv1alpha1.MCPServer{
		serverWithTool(t, "z_tool", "zwrite", "z_type", authz.Readwrite),
		serverWithTool(t, "a_tool", "awrite", "a_type", authz.External),
	}
	first := ConsequentialHandles(servers, nil, nil)
	require.Len(t, first, 2)
	for range 5 {
		assert.Equal(t, first, ConsequentialHandles(servers, nil, nil),
			"the offender list must be deterministic; it lands in a status message re-written on every reconcile")
	}
	assert.Equal(t, []string{"perm:awrite:a_type", "perm:zwrite:z_type"}, first)
}

// TestBuilderClass_TripperSeesTheWorkshopsConsequentialTools is the
// load-bearing pin: the SHIPPED agent-builder, resolved the way the operator's
// containment tripper resolves it.
//
// The builder references no MCPServer and no toolBundle — its workshop
// SidecarToolbox is the ONLY declarer of a tool that can act, and eight of those
// tools are `external`. A leg C that walks only mcpServers and toolkits reads
// "cannot act" for it, and a trifecta that cannot complete leg C never trips.
// That is fail-OPEN on a security path, so the class that actually ships is the
// one worth pinning rather than a fixture shaped to pass.
//
// Namespaces are stamped here because the bundle's manifests carry none: the
// namespace is the installer's to choose, and gatherSidecarToolboxes resolves
// the ref within the class's own.
func TestBuilderClass_TripperSeesTheWorkshopsConsequentialTools(t *testing.T) {
	class, toolbox := builderClassAndToolbox(t)
	class.Namespace = "ns"
	toolbox.Namespace = "ns"

	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(toolbox).Build()

	canAct, err := ClassCanAct(context.Background(), c, class)
	require.NoError(t, err, "the shipped class must resolve; leg C erroring is a tripper that cannot judge")
	assert.True(t, canAct,
		"the builder's workshop sidecar holds eight external tools; leg C reading false here means the trifecta never trips for the one class that ships with this shape")
}

// TestClassCanAct_ASidecarsToolsDecideLegC covers the class shape leg C could
// not see: no mcpServers, no toolBundles, a SidecarToolbox carrying everything.
//
// Resolved through ClassCanAct rather than ConsequentialHandles so the
// RESOLUTION is what is pinned, not just the walk. Dropping the
// gatherSidecarToolboxes call would leave ConsequentialHandles correct and this
// answer wrong, which is exactly how the omission survived.
func TestClassCanAct_ASidecarsToolsDecideLegC(t *testing.T) {
	cases := []struct {
		name   string
		tools  []spiceboxv1alpha1.MCPServerTool
		canAct bool
	}{
		{
			name:   "an external sidecar tool: leg C true, the trifecta can trip",
			tools:  []spiceboxv1alpha1.MCPServerTool{checkedTool("apply", "change", "demo_draft", authz.External)},
			canAct: true,
		},
		{
			name:   "a readwrite sidecar tool: leg C true, the middle tier acts too",
			tools:  []spiceboxv1alpha1.MCPServerTool{checkedTool("write_note", "write", "demo_note", authz.Readwrite)},
			canAct: true,
		},
		{
			name: "only passthrough and stateless sidecar tools: leg C false, nothing here acts",
			tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "get", Permission: &authz.Permission{StateImpact: authz.Passthrough}},
				{Name: "export", Permission: &authz.Permission{StateImpact: authz.Stateless}},
			},
			canAct: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tb := toolboxWithTools(t, "demo-box", tc.tools...)
			c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
				WithObjects(&tb).Build()

			got, err := ClassCanAct(context.Background(), c, classHoldingToolbox(t, "demo-box"))
			require.NoError(t, err)
			assert.Equal(t, tc.canAct, got)
		})
	}
}

// TestClassCanAct_AnUnreadableToolboxIsAnErrorNotAFalseLeg pins the fail-closed
// direction of the read.
//
// Returning false for a toolbox that cannot be read would be the same fail-open
// this change removes, arrived at by a different route: the tripper would judge
// the class harmless because it could not see its tools. The error reaches
// operatorClassCanAct, whose doc says a class that cannot be read is a tripper
// that could not judge.
func TestClassCanAct_AnUnreadableToolboxIsAnErrorNotAFalseLeg(t *testing.T) {
	// No SidecarToolbox object at all, so the ref cannot resolve.
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()

	got, err := ClassCanAct(context.Background(), c, classHoldingToolbox(t, "missing-box"))
	require.Error(t, err, "an unresolvable toolbox must not read as a class that cannot act")
	assert.Contains(t, err.Error(), "sidecarToolboxes",
		"the error must name the source that failed, so an operator knows which read to fix")
	assert.False(t, got, "the bool is meaningless beside an error; it must not be a usable false")
}

// TestConsequentialHandles_NamesASidecarsHandles pins what the offender list
// says about a sidecar tool, both ways it can be named.
//
// The shipped workshop toolbox has both shapes: four external tools keyed on
// workshop_draft, and four more external with no check at all (each raising its
// own card). A walk that reported only the keyed ones would silently drop half
// the reason a class cannot promise it never acts.
func TestConsequentialHandles_NamesASidecarsHandles(t *testing.T) {
	tb := toolboxWithTools(t, "demo-box",
		checkedTool("apply", "change", "demo_draft", authz.External),
		spiceboxv1alpha1.MCPServerTool{
			Name:       "request_credential",
			Permission: &authz.Permission{StateImpact: authz.External},
		},
		spiceboxv1alpha1.MCPServerTool{
			Name:       "get",
			Permission: &authz.Permission{StateImpact: authz.Passthrough},
		},
	)

	assert.Equal(t, []string{"perm:change:demo_draft", "tool:request_credential"},
		ConsequentialHandles(nil, nil, []spiceboxv1alpha1.SidecarToolbox{tb}),
		"a keyed sidecar tool is named by its handle, a checkless one by its tool name, and a passthrough one not at all")
}

// TestCeilingContradictedByASidecarsExternalTool keeps the admission validator
// and the tripper answering the same question.
//
// validateTrifectaCeiling returns early for every class that did not declare
// the ceiling, which is why this blindness was survivable at admission and
// fatal at the tripper — but a class that DOES declare it while holding a
// writing sidecar tool would have been admitted, and its promise is then simply
// false.
func TestCeilingContradictedByASidecarsExternalTool(t *testing.T) {
	ac := classDeclaringNeverConsequential(t, true)
	tb := toolboxWithTools(t, "demo-box", checkedTool("apply", "change", "demo_draft", authz.External))

	reason, msg := validateTrifectaCeiling(ac, nil, nil, []spiceboxv1alpha1.SidecarToolbox{tb})

	assert.Equal(t, spiceboxv1alpha1.ReasonTrifectaCeilingContradicted, reason)
	assert.Contains(t, msg, "perm:change:demo_draft",
		"the offending handle must be named: an operator has to know WHICH permission to remove")
}
