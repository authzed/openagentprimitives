package spiceboxclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/builderbundle"
)

// TestBuilderBundle_ShipsAValidWorkshopSandboxClass closes the gap that kept
// the shipped agent builder from ever becoming Valid on a real cluster: both
// workshop SidecarToolbox manifests set `sandbox.class: workshop-sandbox`,
// but nothing anywhere INSTALLED that SpiceboxClass — the only definitions in
// the tree were e2e fixtures, which also used a shape (`image: ""`, no
// tools) this controller's real validateSpec refuses; the fixtures pass only
// because the e2e harness stamps SpiceboxClasses Valid without ever running
// it. Found live on oap-desktop 2026-09-11: toolbox Valid=False (ClassMissing)
// → builder AgentClass Valid=False → unstartable.
//
// So this test lives HERE, beside validateSpec, and makes two claims:
//
//  1. the builder bundle ships a SpiceboxClass named workshop-sandbox — the
//     bundle carries its own dependency instead of assuming a cluster
//     already has it; and
//  2. that class passes the REAL validateSpec, with empty reservedEnv and
//     nil runtimes (an install-time class references no toolchains and no
//     prewarm runtime) — not a re-transcription of the validator's rules,
//     the validator itself.
func TestBuilderBundle_ShipsAValidWorkshopSandboxClass(t *testing.T) {
	b, err := builderbundle.Bundle()
	require.NoError(t, err)
	crs, err := b.CRs()
	require.NoError(t, err)

	var class *spiceboxv1alpha1.SpiceboxClass
	for _, cr := range crs {
		if cr.GetKind() != "SpiceboxClass" || cr.GetName() != "workshop-sandbox" {
			continue
		}
		require.Nil(t, class, "exactly one workshop-sandbox SpiceboxClass in the bundle")
		var typed spiceboxv1alpha1.SpiceboxClass
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(cr.Object, &typed),
			"the bundle's workshop-sandbox must decode as a SpiceboxClass")
		class = &typed
		// Guard against silent field drops: FromUnstructured ignores unknown
		// fields, so a typo'd key would vanish instead of failing. Spot-check
		// the two facts validateSpec gates on straight off the unstructured.
		img, _, _ := unstructured.NestedString(cr.Object, "spec", "image")
		assert.NotEmpty(t, img, "spec.image must survive decoding")
	}
	require.NotNil(t, class,
		"the builder bundle must SHIP the workshop-sandbox SpiceboxClass its own SidecarToolbox references — "+
			"a referenced-but-never-installed class is why the shipped builder could never become Valid")

	assert.NoError(t, validateSpec(class.Spec, nil, nil),
		"the shipped class must pass the real validator — the e2e harness stamps validity, so this is the only place the real rules run against the shipped shape")
}
