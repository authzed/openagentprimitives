package v1alpha1_test

import (
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests/crdschematest"
)

func TestWorkshopNames(t *testing.T) {
	assert.Equal(t, "s1-workshop", spiceboxv1alpha1.WorkshopName("s1"))
	assert.Equal(t, "s1-workshop-sa", spiceboxv1alpha1.WorkshopServiceAccountName("s1"))
	assert.Equal(t, "s1-workshop-token", spiceboxv1alpha1.WorkshopTokenSecretName("s1"))
	// A UID's hyphens are stripped and only the first 12 hex characters are
	// used, so the namespace name is short, stable, and DNS-safe.
	assert.Equal(t, "ws-a1b2c3d4e5f6",
		spiceboxv1alpha1.WorkshopNamespaceName(types.UID("a1b2c3d4-e5f6-7890-abcd-ef0123456789")))
}

// TestWorkshopInstallAndCapabilityRequestStatus_RoundTrip proves the two
// observed-status sub-objects (status.install, status.capabilityRequest)
// survive a DeepCopy round-trip — which also proves mage gen:api regenerated
// zz_generated.deepcopy.go for the new subtypes (plan 5b task 1). Install is
// written by two disjoint-field owners (the channelsd WorkshopHandoffWatcher
// sets Requested+DeliveredAt; admind sets Installed/Declined/Failed+
// ApprovedBy+InstalledRef); CapabilityRequest is written solely by the
// WorkshopHandoffWatcher.
func TestWorkshopInstallAndCapabilityRequestStatus_RoundTrip(t *testing.T) {
	requestedAt := metav1.NewTime(time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC))
	deliveredAt := metav1.NewTime(time.Date(2026, 9, 4, 10, 5, 0, 0, time.UTC))
	capDeliveredAt := metav1.NewTime(time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC))

	w := &spiceboxv1alpha1.Workshop{Status: spiceboxv1alpha1.WorkshopStatus{
		Install: &spiceboxv1alpha1.WorkshopInstallStatus{
			Phase:        "Installed",
			RequestedAt:  &requestedAt,
			DeliveredAt:  &deliveredAt,
			ApprovedBy:   "user:demo-admin",
			InstalledRef: "agents/weather-ai",
			Message:      "installed successfully",
		},
		CapabilityRequest: &spiceboxv1alpha1.WorkshopCapabilityRequestStatus{
			DeliveredAt: &capDeliveredAt,
			NoticeRef:   "req-2",
		},
	}}

	got := w.DeepCopy()

	require.NotNil(t, got.Status.Install)
	assert.Equal(t, "Installed", got.Status.Install.Phase)
	assert.Equal(t, requestedAt, *got.Status.Install.RequestedAt)
	assert.Equal(t, deliveredAt, *got.Status.Install.DeliveredAt)
	assert.Equal(t, "user:demo-admin", got.Status.Install.ApprovedBy)
	assert.Equal(t, "agents/weather-ai", got.Status.Install.InstalledRef)
	assert.Equal(t, "installed successfully", got.Status.Install.Message)

	require.NotNil(t, got.Status.CapabilityRequest)
	assert.Equal(t, capDeliveredAt, *got.Status.CapabilityRequest.DeliveredAt)
	assert.Equal(t, "req-2", got.Status.CapabilityRequest.NoticeRef)
}

func TestBuilderClassFor(t *testing.T) {
	list := []spiceboxv1alpha1.BuilderClassRef{
		{Namespace: "agents", Name: "builder", SidecarToolbox: "workshop"},
	}
	l := &spiceboxv1alpha1.SettingsLimits{BuilderClasses: &list}
	assert.NotNil(t, l.BuilderClassFor("agents", "builder"))
	assert.Nil(t, l.BuilderClassFor("agents", "other"), "unlisted class: no sanction")
	assert.Nil(t, l.BuilderClassFor("other", "builder"), "same name, other namespace: no sanction")
	assert.Nil(t, (*spiceboxv1alpha1.SettingsLimits)(nil).BuilderClassFor("agents", "builder"), "nil limits fail closed")
	empty := &spiceboxv1alpha1.SettingsLimits{}
	assert.Nil(t, empty.BuilderClassFor("agents", "builder"), "absent list fails closed")
}

// TestWorkshopCloseTarget_GrammarIsPinnedInTheGeneratedCRD reads the target
// grammar out of the SHIPPED install bundle rather than off the Go marker it
// is supposed to be checking (the same discipline as
// TestAgentSkill_TargetDefaultsToAgentInTheGeneratedCRD): deleting the
// +kubebuilder:validation:Pattern and regenerating leaves this test red, where
// an assertion on the marker's own text would stay green whatever the CRD
// ended up saying.
//
// The pattern is the only thing standing between the close pass and a target
// it cannot resolve — a name with a second "/" in it, an empty namespace, a
// "*:" with no number — so every shape the controller's parse assumes is
// pinned here, in both directions.
func TestWorkshopCloseTarget_GrammarIsPinnedInTheGeneratedCRD(t *testing.T) {
	schema := crdschematest.FieldSchema(t, "workshops", "spec.closeRequests.items.properties.target")

	require.NotEmpty(t, schema.Pattern, "the close target must carry a pattern in the generated CRD")
	require.NotNil(t, schema.MaxLength, "the close target must be length-bounded in the generated CRD")
	assert.Equal(t, int64(127), *schema.MaxLength)

	re, err := regexp.Compile(schema.Pattern)
	require.NoError(t, err, "the generated pattern must compile: %s", schema.Pattern)

	cases := []struct {
		target string
		want   bool
	}{
		{target: "agent-builder-x-workshop", want: true},
		{target: "x", want: true},
		{target: "demo-ns/agent-builder-x-workshop", want: true},
		{target: "0/0", want: true},
		{target: spiceboxv1alpha1.WorkshopCloseTargetAll, want: true},
		{target: "*:2", want: true},
		{target: "*:10", want: true},
		{target: "", want: false},
		{target: "Agent-Builder-X", want: false},
		{target: "agent_builder", want: false},
		{target: "-x", want: false},
		{target: "x-", want: false},
		{target: "demo-ns/", want: false},
		{target: "/agent-builder-x", want: false},
		{target: "demo-ns/sub/agent-builder-x", want: false},
		{target: "demo ns/x", want: false},
		{target: "*:0", want: false},
		{target: "*:", want: false},
		{target: "*:2x", want: false},
		{target: "**", want: false},
		{target: "*/x", want: false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, re.MatchString(tc.target), "target %q", tc.target)
	}
}

// TestIsWorkshopCloseWildcard_AgreesWithTheGeneratedCRDPattern pins the one
// grammar the wildcard has. The controller asks IsWorkshopCloseWildcard
// whether to EXPAND a target, and the close_others tool asks it what key its
// next ask may take; both answers are only safe if they match the pattern the
// apiserver admitted the target under. Two hand-written copies of "*:<n>" is
// how a target gets admitted as an ask and then read as a workshop name — a
// Get for a name no workshop can hold.
//
// The pattern is read out of the shipped bundle, not off the Go marker, for
// the same reason the grammar test above does it: a regenerated CRD that
// stopped saying this would leave an assertion on the marker's own text green.
func TestIsWorkshopCloseWildcard_AgreesWithTheGeneratedCRDPattern(t *testing.T) {
	schema := crdschematest.FieldSchema(t, "workshops", "spec.closeRequests.items.properties.target")
	re, err := regexp.Compile(schema.Pattern)
	require.NoError(t, err, "the generated pattern must compile: %s", schema.Pattern)

	cases := []struct {
		target string
		want   bool
	}{
		{target: spiceboxv1alpha1.WorkshopCloseTargetAll, want: true},
		{target: "*:2", want: true},
		{target: "*:10", want: true},
		{target: "*:0", want: false},
		{target: "*:", want: false},
		{target: "*:abc", want: false},
		{target: "**", want: false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, spiceboxv1alpha1.IsWorkshopCloseWildcard(tc.target),
			"IsWorkshopCloseWildcard(%q)", tc.target)
		assert.Equal(t, tc.want, re.MatchString(tc.target),
			"the CRD pattern must admit exactly the wildcards the func reads: %q", tc.target)
	}
}
