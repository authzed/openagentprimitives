//go:build e2e

package bronzethread_test

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestWorkshopInventoryFixture_MatchesDeployToolSet guards the THIRD
// hand-maintained copy of the workshop sidecar's tool declaration.
//
// The workshop sidecar's tool set exists in three places: this bundle's own
// 00-agent.yaml fixture, pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml
// (the shipped manifest), and pkg/platform/builderbundle's
// src/manifests/sidecartoolbox.yaml (the .oap-assembled copy). The latter
// two are compared to each other by TestSidecarToolbox_MatchesDeployToolSet
// (pkg/platform/builderbundle/embed_test.go); that comparison lives in a
// pkg/ unit-test package and cannot reach into a test/e2e testdata path
// without making pkg/ depend on a test fixture, so this fixture's copy was
// never checked against anything. This bundle's own e2e suite is the right
// home for that check instead: it already imports the deploy-side types,
// and this bundle already exists specifically to exercise the workshop
// sidecar's real tool surface.
//
// This fixture's own header comment claims its SidecarToolbox spec is
// "copied VERBATIM from pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml" --
// a claim nothing verified before this test. It was false on two counts:
// the `inventory` tool's intent had gone stale (missing the "first-party
// sidecar images this cluster admits" clause -- the very capability
// bundle.json's third `workshop_inventory` assertion, op-3, exists to
// prove), and the fixture dropped two whole tools present in every other
// copy (agents_in_thread, project_agent). See
// 2026-09-10-agent-builder-plan-8c task 1.
// A TABLE, deliberately: plan 8c fixed a "third copy nothing compares"
// defect and then, two commits later, grew a FOURTH uncovered copy in
// workshop-apply-earns-a-real-verdict's own fixture. Every bundle that
// ships the workshop toolbox belongs on this list, so the next one is a
// row rather than a rediscovery.
func TestWorkshopFixtures_MatchDeployToolSet(t *testing.T) {
	for _, fixture := range []string{
		"testdata/workshop-inventory-is-real/manifests/00-agent.yaml",
		"testdata/workshop-apply-earns-a-real-verdict/manifests/00-agent.yaml",
		"testdata/workshop-recommend-capability-is-real/manifests/00-agent.yaml",
		"testdata/workshop-try-it-link-and-watch/manifests/00-agent.yaml",
		"testdata/workshop-draft-slot-narrows-to-the-phase/manifests/00-agent.yaml",
	} {
		t.Run(fixture, func(t *testing.T) { assertFixtureMatchesDeploy(t, fixture) })
	}
}

func assertFixtureMatchesDeploy(t *testing.T, fixturePath string) {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	require.NoError(t, err, "read the fixture manifest")

	var sidecarToolboxes int
	var fixtureSpec *spiceboxv1alpha1.SidecarToolboxSpec
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096)
	for {
		var doc map[string]any
		derr := dec.Decode(&doc)
		if derr == io.EOF {
			break
		}
		require.NoError(t, derr, "decode a YAML document from the fixture manifest")
		if len(doc) == 0 {
			continue // empty doc separator
		}
		if kind, _ := doc["kind"].(string); kind != "SidecarToolbox" {
			continue
		}
		sidecarToolboxes++
		specBytes, merr := json.Marshal(doc["spec"])
		require.NoError(t, merr, "marshal the SidecarToolbox doc's spec back to JSON")
		var spec spiceboxv1alpha1.SidecarToolboxSpec
		require.NoError(t, json.Unmarshal(specBytes, &spec), "decode spec as SidecarToolboxSpec")
		fixtureSpec = &spec
	}
	// Exactly one, not merely "found one": the loop above ASSIGNS
	// fixtureSpec rather than accumulating, so a fixture carrying a second
	// SidecarToolbox doc would silently compare only the last one decoded.
	require.Equal(t, 1, sidecarToolboxes, "%s must declare exactly one SidecarToolbox", fixturePath)
	require.NotNil(t, fixtureSpec, "the fixture manifest must declare a SidecarToolbox")

	deployData, err := os.ReadFile("../../../pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml")
	require.NoError(t, err, "read the deploy-side declaration")
	var deploySpec spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(deployData, &deploySpec))

	fixtureTools := make(map[string]spiceboxv1alpha1.MCPServerTool, len(fixtureSpec.Tools))
	for _, tool := range fixtureSpec.Tools {
		fixtureTools[tool.Name] = tool
	}
	deployTools := make(map[string]spiceboxv1alpha1.MCPServerTool, len(deploySpec.Tools))
	for _, tool := range deploySpec.Tools {
		deployTools[tool.Name] = tool
	}

	assert.Equal(t, deployTools, fixtureTools,
		"%s's SidecarToolbox spec.tools must match pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml "+
			"verbatim, per that fixture's own header comment", fixturePath)

	// "Verbatim" is the fixtures' own word and it is not quite true, so say
	// where: all five omit `isolation: isolated`, which the deploy copy and the
	// bundle copy both carry (TestSidecarToolbox_DeclaresIsolatedInBothCopies
	// pins those two). This comparison is scoped to spec.tools and
	// spec.spicedbSchema and does not reach it — deliberately, since separate-pod
	// isolation is about how a real sidecar comes by its projected identity and
	// this harness mounts the workshop server directly.

	// spec.spicedbSchema is compared alongside the tools, because four of
	// those tools carry a permission.check naming `workshop_draft` and a check
	// is inert without the fragment that DECLARES that resource: composition
	// would find no such definition, every slot would be skipped, and the
	// builder would silently go back to a card per apply. The tool comparison
	// above cannot see that — spicedbSchema is not part of spec.tools — so a
	// fixture that copied the checks and not the resource would pass it.
	assert.Equal(t, deploySpec.SpiceDBSchema, fixtureSpec.SpiceDBSchema,
		"%s's SidecarToolbox spec.spicedbSchema must match the deploy declaration: the resource its "+
			"tools' checks name has to ship with them", fixturePath)
}
