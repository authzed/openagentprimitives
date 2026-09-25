package capability

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
)

// TestChannelFeaturesTableMirrorsTheRegistry is the anti-drift guard for the
// static table in pkg/channels/channelfeatures.
//
// That table cannot be populated by registration: internal/cmd/channelsd computes
// ScopesValid but does not import this package, so an init-based registry
// would be empty in the one binary that matters. The table is therefore
// hand-written, and THIS test is what keeps it honest. Do not delete or
// weaken it — the failure mode it prevents is silent: a renamed capability
// simply stops requiring its transport scopes, and nothing else notices.
func TestChannelFeaturesTableMirrorsTheRegistry(t *testing.T) {
	registered := map[string]Capability{}
	for _, c := range Ordered() {
		registered[c.Name()] = c
	}
	require.NotEmpty(t, registered, "the capability registry must be populated by init")

	for name, defaultOn := range channelfeatures.Capabilities() {
		t.Run(name+": exists in the registry with a matching DefaultOn", func(t *testing.T) {
			c, ok := registered[name]
			require.True(t, ok,
				"channelfeatures names capability %q, which is not registered; "+
					"it was probably renamed or removed without updating pkg/channels/channelfeatures/table.go", name)
			assert.Equal(t, c.DefaultOn(), defaultOn,
				"channelfeatures records defaultOn=%v for %q but the registry says %v; "+
					"a disagreement means the wizard and the runtime resolve different feature sets",
				defaultOn, name, c.DefaultOn())
		})
	}
}

// TestEveryChannelFeatureIsReachable asserts no Feature is declared that no
// capability can ever activate — a dead feature would quietly widen the
// required-scope set for nobody's benefit.
func TestEveryChannelFeatureIsReachable(t *testing.T) {
	reachable := map[channelfeatures.Feature]bool{}
	for _, f := range channelfeatures.Baseline() {
		reachable[f] = true
	}

	// Build a class granting every mapped capability, then resolve.
	class := allCapabilitiesClass(t)
	active, err := channelfeatures.ActiveFor(class)
	require.NoError(t, err)
	for _, f := range active {
		reachable[f] = true
	}

	for _, f := range channelfeatures.All() {
		assert.True(t, reachable[f],
			"Feature %q is declared but no capability (and not the baseline) activates it; "+
				"either wire it into a pkg/channels/channelfeatures/table.go entry or remove it from pkg/channels/channelfeatures/feature.go", f)
	}
}

func allCapabilitiesClass(t *testing.T) *v1alpha1.AgentClass {
	t.Helper()
	c := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "demo-ns"},
	}
	c.Spec.Capabilities = map[string]apiextensionsv1.JSON{}
	for name := range channelfeatures.Capabilities() {
		c.Spec.Capabilities[name] = apiextensionsv1.JSON{Raw: []byte(`{}`)}
	}
	return c
}
