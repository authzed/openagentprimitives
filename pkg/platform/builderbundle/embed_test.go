package builderbundle_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/builderbundle"
)

// TestBuilderBundle_AssemblesAndValidates proves the embedded builder .oap
// source assembles into a validated bundle: the AgentClass lockdown, the
// SidecarToolbox tool set, the AgentUI stub and the eight builder-phase
// skills all decode as CRs a real oap install could apply. A failure here
// means either the assembler broke or one of the hand-authored manifests
// drifted out of the allowed-kind / binding-target shape Bundle.Validate
// enforces.
func TestBuilderBundle_AssemblesAndValidates(t *testing.T) {
	b, err := builderbundle.Bundle()
	require.NoError(t, err)
	require.NoError(t, b.Validate(), "the builder bundle is a valid .oap")

	crs, err := b.CRs()
	require.NoError(t, err)
	kinds := map[string]int{}
	for _, cr := range crs {
		kinds[cr.GetKind()]++
	}
	assert.Equal(t, 1, kinds["AgentClass"])
	assert.Equal(t, 1, kinds["SidecarToolbox"])
	assert.Equal(t, 1, kinds["AgentUI"])
	assert.Equal(t, 8, kinds["Skill"], "eight builder-phase skills, folded from SKILL.md")
}

// TestSidecarToolbox_MatchesDeployToolSet is MINOR-12: this bundle's
// src/manifests/sidecartoolbox.yaml and
// pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml are two SEPARATE,
// hand-edited files describing the same workshop sidecar (the deploy copy is
// a flat SidecarToolboxSpec the sidecar's own kind validates against a live
// server; this bundle's copy is the same content wrapped as a real CR for
// `.oap` assembly — see this bundle's own sidecartoolbox.yaml header). Only
// the deploy copy was ever checked against anything (deploy/sidecartoolbox_test.go's
// wantTools). Nothing compared the two to each other, so an edit to one
// tool set that forgot the other would pass every existing test.
//
// This compares the full per-tool declaration each side carries — name,
// intent, effects and permission — not just tool names. A names-only
// comparison already let a model-facing description drift: the deploy
// copy's `inventory` tool lost the "first-party sidecar images this cluster
// admits" clause from its intent while keeping the same name, and that
// passed the old (names-only) version of this test. See
// 2026-09-10-agent-builder-plan-8c task 1.
func TestSidecarToolbox_MatchesDeployToolSet(t *testing.T) {
	b, err := builderbundle.Bundle()
	require.NoError(t, err)
	crs, err := b.CRs()
	require.NoError(t, err)

	var sidecarToolboxes int
	var bundleTools map[string]spiceboxv1alpha1.MCPServerTool
	for _, cr := range crs {
		if cr.GetKind() != "SidecarToolbox" {
			continue
		}
		sidecarToolboxes++
		tools, _, err := unstructured.NestedSlice(cr.Object, "spec", "tools")
		require.NoError(t, err, "read spec.tools from the bundle's SidecarToolbox CR")
		bundleTools = toolFactsByName(t, tools)
	}
	// Exactly one, not merely "at least one": the loop above ASSIGNS
	// bundleTools rather than accumulating, so a bundle carrying a second
	// SidecarToolbox CR would silently compare only the last one seen and
	// never notice the first went unchecked.
	require.Equal(t, 1, sidecarToolboxes, "the bundle must declare exactly one SidecarToolbox")
	require.NotEmpty(t, bundleTools, "the bundle must declare a SidecarToolbox with at least one tool")

	deployData, err := os.ReadFile("../../tools/workshopmcp/deploy/sidecartoolbox.yaml")
	require.NoError(t, err, "read the deploy-side declaration")
	var deploySpec spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(deployData, &deploySpec))
	deployTools := make(map[string]spiceboxv1alpha1.MCPServerTool, len(deploySpec.Tools))
	for _, tool := range deploySpec.Tools {
		deployTools[tool.Name] = tool
	}

	assert.Equal(t, deployTools, bundleTools,
		"the bundle's SidecarToolbox and pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml are hand-edited "+
			"independently and must declare the SAME tools — same names AND same intent/effects/permission")
}

// toolFactsByName decodes an unstructured spec.tools slice (as read off a
// SidecarToolbox CR) into a map keyed by tool name, one MCPServerTool per
// entry — the same typed shape the deploy-side flat spec already decodes
// into, so the two sides compare like for like.
func toolFactsByName(t *testing.T, tools []any) map[string]spiceboxv1alpha1.MCPServerTool {
	t.Helper()
	out := make(map[string]spiceboxv1alpha1.MCPServerTool, len(tools))
	for _, raw := range tools {
		m, ok := raw.(map[string]any)
		require.True(t, ok, "spec.tools entry must be a map, got %T", raw)
		b, err := json.Marshal(m)
		require.NoError(t, err, "marshal spec.tools entry back to JSON")
		var tool spiceboxv1alpha1.MCPServerTool
		require.NoError(t, json.Unmarshal(b, &tool), "decode spec.tools entry as MCPServerTool")
		require.NotEmpty(t, tool.Name, "spec.tools entry must carry a name")
		_, dup := out[tool.Name]
		require.False(t, dup, "duplicate tool name %q in bundle SidecarToolbox", tool.Name)
		out[tool.Name] = tool
	}
	return out
}

// TestSidecarToolbox_DeclaresIsolatedInBothCopies pins `isolation: isolated`
// on BOTH hand-edited copies of the workshop toolbox (this bundle's CR and
// pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml — see
// TestSidecarToolbox_MatchesDeployToolSet for why two copies exist at all,
// and why each new spec-level fact must be asserted on both).
//
// Why isolated is load-bearing and not a preference: the ap-workshop image
// is fail-closed on the projected workshop SA token, and BuildSidecarPod's
// identity branch — the ONLY thing that projects that token — is reachable
// solely in separate-pod mode (RunModeFor). An in-pod workshop sidecar
// boots without its identity and dies exactly the way the admission probe
// did live on oap-desktop 2026-09-11. Plan 0a made isolation declarable and
// deliberately left the workshop opt-in unmade; the first live install made
// it load-bearing.
func TestSidecarToolbox_DeclaresIsolatedInBothCopies(t *testing.T) {
	b, err := builderbundle.Bundle()
	require.NoError(t, err)
	crs, err := b.CRs()
	require.NoError(t, err)

	found := false
	for _, cr := range crs {
		if cr.GetKind() != "SidecarToolbox" {
			continue
		}
		found = true
		iso, _, err := unstructured.NestedString(cr.Object, "spec", "isolation")
		require.NoError(t, err)
		assert.Equal(t, "isolated", iso,
			"bundle copy: the workshop toolbox must run separate-pod or the identity branch never projects its SA token")
	}
	require.True(t, found, "the bundle must declare a SidecarToolbox")

	deployData, err := os.ReadFile("../../tools/workshopmcp/deploy/sidecartoolbox.yaml")
	require.NoError(t, err)
	var deploySpec spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(deployData, &deploySpec))
	assert.Equal(t, spiceboxv1alpha1.SidecarToolboxIsolationIsolated, deploySpec.Isolation,
		"deploy copy: same fact, same reason — the two files are hand-edited independently")
}

// TestSidecarToolbox_DeclaresTheSameDraftResourceInBothCopies covers the other
// half of the draft ruling. TestSidecarToolbox_MatchesDeployToolSet compares
// spec.tools, so the four `permission.check` blocks are already pinned across
// both copies — but a check naming `workshop_draft` is inert unless the
// fragment that DECLARES that resource ships with it, and spec.spicedbSchema
// is not part of spec.tools. A bundle that carried the checks and lost the
// resource would compose a schema with no `workshop_draft` definition at all,
// every slot would be skipped, and every call would silently go back to a card
// per call — the exact state this plan removed, reintroduced with no test
// failing.
func TestSidecarToolbox_DeclaresTheSameDraftResourceInBothCopies(t *testing.T) {
	b, err := builderbundle.Bundle()
	require.NoError(t, err)
	crs, err := b.CRs()
	require.NoError(t, err)

	var bundleSchema *spiceboxv1alpha1.SpiceDBSchemaFragment
	found := false
	for _, cr := range crs {
		if cr.GetKind() != "SidecarToolbox" {
			continue
		}
		found = true
		raw, _, nerr := unstructured.NestedMap(cr.Object, "spec", "spicedbSchema")
		require.NoError(t, nerr, "read spec.spicedbSchema from the bundle's SidecarToolbox CR")
		require.NotNil(t, raw, "the bundle copy must declare the resource its tools' checks name")
		encoded, merr := json.Marshal(raw)
		require.NoError(t, merr)
		var frag spiceboxv1alpha1.SpiceDBSchemaFragment
		require.NoError(t, json.Unmarshal(encoded, &frag), "decode spec.spicedbSchema as a SpiceDBSchemaFragment")
		bundleSchema = &frag
	}
	require.True(t, found, "the bundle must declare a SidecarToolbox")

	deployData, err := os.ReadFile("../../tools/workshopmcp/deploy/sidecartoolbox.yaml")
	require.NoError(t, err)
	var deploySpec spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(deployData, &deploySpec))
	require.NotNil(t, deploySpec.SpiceDBSchema, "the deploy copy must declare the resource its tools' checks name")

	assert.Equal(t, deploySpec.SpiceDBSchema, bundleSchema,
		"the bundle's SidecarToolbox and pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml are hand-edited "+
			"independently and must declare the SAME spicedbSchema — same resource, standing, relations and display")
}

// TestBuilderClass_DeclaresItsOwnBudget: a class with NO spec.budget on a
// cluster whose settings tiers default none resolves an all-zero effective
// budget, and the status stamp then violates BudgetConfig's CRD floors
// (Minimum=1) — the apiserver rejects EVERY status write, the class wedges at
// its last-written conditions, and the failure is invisible except in
// operator logs. Found live on oap-desktop 2026-09-11: the builder was the
// first shipped class with no budget (every e2e fixture declares one, so the
// suites never met the shape). The platform-level fix — what an undeclared
// budget should MEAN — is a recorded design decision; the bundle does not
// wait for it: the builder declares its own, like every other working
// class does.
func TestBuilderClass_DeclaresItsOwnBudget(t *testing.T) {
	b, err := builderbundle.Bundle()
	require.NoError(t, err)
	crs, err := b.CRs()
	require.NoError(t, err)

	for _, cr := range crs {
		if cr.GetKind() != "AgentClass" {
			continue
		}
		budget, found, err := unstructured.NestedMap(cr.Object, "spec", "budget")
		require.NoError(t, err)
		require.True(t, found, "the builder AgentClass must declare spec.budget — see the doc above for why absence wedges the class")
		for _, dim := range []string{"maxTurns", "maxTokens"} {
			// YAML decoding yields int64 or float64 depending on the path
			// the bundle took to unstructured; the FACT is positivity.
			var v int64
			switch n := budget[dim].(type) {
			case int64:
				v = n
			case float64:
				v = int64(n)
			default:
				t.Fatalf("spec.budget.%s must be set (got %T)", dim, budget[dim])
			}
			assert.Positive(t, v, "spec.budget.%s must satisfy the CRD floor", dim)
		}
		assert.Equal(t, "168h", budget["sessionExpiration"],
			"the builder session's wall-clock cap tracks the workshop's own maxAge default (7 days, design spec §12): the workshop namespace dies at 7d, so a builder session outliving it could only fail")
		return
	}
	t.Fatal("no AgentClass in the builder bundle")
}
