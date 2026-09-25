package workshopmcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// TestRequestInstall_SetsInstallRequestOnce is request_install's happy path:
// it sets spec.installRequest on the Workshop CR living in the BUILDER
// session's namespace (not the workshop namespace W).
func TestRequestInstall_SetsInstallRequestOnce(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	res := callTool(t, s.handleRequestInstall, installArgs{SuggestedName: "demo-agent", BundleDigest: "sha256:abc"})
	require.False(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Equal(t, "demo-agent", body["suggestedName"])
	assert.Equal(t, "requested", body["status"])

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	require.NotNil(t, got.Spec.InstallRequest)
	assert.Equal(t, "demo-agent", got.Spec.InstallRequest.SuggestedName)
	assert.Equal(t, "sha256:abc", got.Spec.InstallRequest.BundleDigest)
}

// TestRequestInstall_SecondCallRefused proves the once-only discipline: a
// second request_install after one is already recorded is refused with a
// tool error, and the ORIGINAL field is left unchanged — never overwritten.
func TestRequestInstall_SecondCallRefused(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	first := callTool(t, s.handleRequestInstall, installArgs{SuggestedName: "demo-agent"})
	require.False(t, first.IsError)

	second := callTool(t, s.handleRequestInstall, installArgs{SuggestedName: "other-agent"})
	require.True(t, second.IsError)
	body := decodeResultBody(t, second)
	assert.Contains(t, body["error"], "already been requested")

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	require.NotNil(t, got.Spec.InstallRequest)
	assert.Equal(t, "demo-agent", got.Spec.InstallRequest.SuggestedName, "second call must not overwrite the first request")
}

// TestRequestInstall_RequiresSuggestedName pins the argument guard.
func TestRequestInstall_RequiresSuggestedName(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	res := callTool(t, s.handleRequestInstall, installArgs{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "suggestedName is required")

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	assert.Nil(t, got.Spec.InstallRequest, "a rejected call must write nothing to the Workshop CR")
}

// TestRecommendCapability_SetsCapabilityRequestOnce is recommend_capability's
// happy path.
func TestRecommendCapability_SetsCapabilityRequestOnce(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	res := callTool(t, s.handleRecommendCapability, capabilityArgs{
		Summary: "needs a weather lookup toolbox", ArtifactRef: "agents/weather-ai/probe-1",
	})
	require.False(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Equal(t, "needs a weather lookup toolbox", body["summary"])
	assert.Equal(t, "recommended", body["status"])

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	require.NotNil(t, got.Spec.CapabilityRequest)
	assert.Equal(t, "needs a weather lookup toolbox", got.Spec.CapabilityRequest.Summary)
	assert.Equal(t, "agents/weather-ai/probe-1", got.Spec.CapabilityRequest.ArtifactRef)
}

// TestRecommendCapability_SecondCallRefused proves the once-only discipline
// for recommend_capability: a second call after one is already recorded is
// refused, and the original field is left unchanged.
func TestRecommendCapability_SecondCallRefused(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	first := callTool(t, s.handleRecommendCapability, capabilityArgs{Summary: "first summary", ArtifactRef: "agents/weather-ai/probe-1"})
	require.False(t, first.IsError)

	second := callTool(t, s.handleRecommendCapability, capabilityArgs{Summary: "second summary", ArtifactRef: "agents/weather-ai/probe-2"})
	require.True(t, second.IsError)
	body := decodeResultBody(t, second)
	assert.Contains(t, body["error"], "already been recommended")

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	require.NotNil(t, got.Spec.CapabilityRequest)
	assert.Equal(t, "first summary", got.Spec.CapabilityRequest.Summary, "second call must not overwrite the first recommendation")
}

// TestRecommendCapability_RequiresSummaryAndArtifactRef pins the argument
// guard: both summary and artifactRef are required.
func TestRecommendCapability_RequiresSummaryAndArtifactRef(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	res := callTool(t, s.handleRecommendCapability, capabilityArgs{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "summary and artifactRef are required")

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	assert.Nil(t, got.Spec.CapabilityRequest, "a rejected call must write nothing to the Workshop CR")
}
