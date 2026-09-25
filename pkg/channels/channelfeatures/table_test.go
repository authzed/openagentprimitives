package channelfeatures

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func classWith(caps map[string]string) *v1alpha1.AgentClass {
	c := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "demo-ns"}}
	if caps == nil {
		return c
	}
	c.Spec.Capabilities = map[string]apiextensionsv1.JSON{}
	for k, v := range caps {
		c.Spec.Capabilities[k] = apiextensionsv1.JSON{Raw: []byte(v)}
	}
	return c
}

func TestActiveForNilClassYieldsBaseline(t *testing.T) {
	got, err := ActiveFor(nil)
	require.NoError(t, err, "a nil class is the monitoring-channel case, not an error")
	assert.ElementsMatch(t, Baseline(), got)
}

func TestActiveForIncludesDefaultOnCapabilitiesWithoutAGrant(t *testing.T) {
	// thread_history is DefaultOn, so an AgentClass that lists nothing still
	// gets its features. This is the property that keeps existing installs
	// working after the wizard starts writing an explicit capability map.
	got, err := ActiveFor(classWith(nil))
	require.NoError(t, err)
	assert.Contains(t, got, ThreadHistory, "a default-on capability needs no grant")
	assert.NotContains(t, got, AttachmentsOutbound, "an opt-in capability must NOT be active without a grant")
}

func TestActiveForHonorsOptInGrants(t *testing.T) {
	got, err := ActiveFor(classWith(map[string]string{"attachments": `{}`}))
	require.NoError(t, err)
	assert.Contains(t, got, AttachmentsOutbound)
	assert.Contains(t, got, AttachmentsInbound)
}

func TestActiveForHonorsExplicitDisable(t *testing.T) {
	got, err := ActiveFor(classWith(map[string]string{"thread_history": `{"enabled":false}`}))
	require.NoError(t, err)
	assert.NotContains(t, got, ThreadHistory, "enabled:false must deactivate a default-on capability")
}

func TestActiveForMalformedConfigFailsClosedAndReturnsError(t *testing.T) {
	got, err := ActiveFor(classWith(map[string]string{"attachments": `{"enabled":`}))
	require.Error(t, err, "a malformed capability value must not be silently dropped")
	assert.NotContains(t, got, AttachmentsOutbound, "malformed config fails CLOSED")
}

func TestActiveForResultIsDeduplicatedAndStable(t *testing.T) {
	// attachments and artifacts both map to AttachmentsOutbound — granting
	// both is the only real collision in the table, so this is what actually
	// exercises the dedup logic (granting two capabilities with disjoint
	// feature sets would leave nothing to deduplicate).
	got, err := ActiveFor(classWith(map[string]string{
		"attachments": `{}`,
		"artifacts":   `{}`,
	}))
	require.NoError(t, err)

	assert.Contains(t, got, AttachmentsOutbound,
		"attachments and artifacts both grant AttachmentsOutbound; it must still be present")

	seen := map[Feature]int{}
	for _, f := range got {
		seen[f]++
	}
	for f, n := range seen {
		assert.Equal(t, 1, n, "feature %q appeared %d times; result must be deduplicated", f, n)
	}

	again, err := ActiveFor(classWith(map[string]string{
		"attachments": `{}`,
		"artifacts":   `{}`,
	}))
	require.NoError(t, err)
	assert.Equal(t, got, again, "order must be stable across calls (no map iteration leaking out)")
}

func TestBaselineIsASubsetOfAll(t *testing.T) {
	all := map[Feature]bool{}
	for _, f := range All() {
		all[f] = true
	}
	for _, f := range Baseline() {
		assert.True(t, all[f], "baseline feature %q must be in All()", f)
	}
}

func TestFeaturesForKnownCapability(t *testing.T) {
	got := FeaturesFor("attachments")
	assert.ElementsMatch(t, []Feature{AttachmentsOutbound, AttachmentsInbound}, got)
}

func TestFeaturesForUnknownCapabilityIsNil(t *testing.T) {
	assert.Nil(t, FeaturesFor("no-such-capability"),
		"an unknown capability implies no features; callers range over the result")
}

func TestFeaturesForReturnsACopy(t *testing.T) {
	got := FeaturesFor("attachments")
	require.NotEmpty(t, got)
	got[0] = "mutated"
	assert.NotContains(t, FeaturesFor("attachments"), Feature("mutated"),
		"FeaturesFor must not hand out the table's own slice")
}

// Pins the two accessors against one table: every capability Capabilities()
// reports must resolve to the right feature set. Asserting identity rather than
// presence is what catches a swapped or copy-pasted feature list.
func TestFeaturesForAgreesWithCapabilities(t *testing.T) {
	caps := Capabilities()
	require.NotEmpty(t, caps, "Capabilities() must not be empty; if it is, the test runs zero subtests and vacuously passes")

	// The expected mappings, independent of table.go so a wrong entry there
	// fails here.
	cases := []struct {
		capability   string
		wantFeatures []Feature
	}{
		{capability: "attachments", wantFeatures: []Feature{AttachmentsOutbound, AttachmentsInbound}},
		{capability: "artifacts", wantFeatures: []Feature{AttachmentsOutbound}},
		{capability: "thread_history", wantFeatures: []Feature{ThreadHistory}},
		{capability: "channel_history", wantFeatures: []Feature{ChannelHistory}},
		{capability: "mention_lookup", wantFeatures: []Feature{UserLookup}},
		{capability: "credential_update", wantFeatures: []Feature{CredentialPortal}},
		{capability: "session_views", wantFeatures: []Feature{SessionViews}},
	}

	for _, tc := range cases {
		t.Run(tc.capability+": returns correct features", func(t *testing.T) {
			require.Contains(t, caps, tc.capability, "test error: expected capability not in Capabilities()")
			got := FeaturesFor(tc.capability)
			assert.ElementsMatch(t, tc.wantFeatures, got,
				"FeaturesFor(%q) must return exactly %v, not %v", tc.capability, tc.wantFeatures, got)
		})
	}
}
