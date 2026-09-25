package workshop

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestBuildWorkshopLimitRange is the pure-builder half of the pairing
// BuildWorkshopQuota forces: the quota demands requests AND limits on every
// container, so a namespace with no LimitRange to default them refuses any pod
// whose author did not set all four by hand — which is every pod a person
// starts to try what the builder built. The defaults asserted here are the ones
// that make such a pod admissible.
func TestBuildWorkshopLimitRange_DefaultsEveryContainerRequestAndLimit(t *testing.T) {
	lr := BuildWorkshopLimitRange("ws-demo")

	require.NotNil(t, lr)
	assert.Equal(t, "v1", lr.APIVersion)
	assert.Equal(t, "LimitRange", lr.Kind)
	assert.Equal(t, "ws-demo", lr.Namespace)
	assert.Equal(t, "workshop-limits", lr.Name)

	require.Len(t, lr.Spec.Limits, 1, "one Container item: the quota bounds containers, not pods")
	item := lr.Spec.Limits[0]
	assert.Equal(t, corev1.LimitTypeContainer, item.Type)

	assert.True(t, resource.MustParse("250m").Equal(item.DefaultRequest[corev1.ResourceCPU]),
		"defaultRequest.cpu, got %v", item.DefaultRequest[corev1.ResourceCPU])
	assert.True(t, resource.MustParse("512Mi").Equal(item.DefaultRequest[corev1.ResourceMemory]),
		"defaultRequest.memory, got %v", item.DefaultRequest[corev1.ResourceMemory])
	assert.True(t, resource.MustParse("1").Equal(item.Default[corev1.ResourceCPU]),
		"default.cpu, got %v", item.Default[corev1.ResourceCPU])
	assert.True(t, resource.MustParse("2Gi").Equal(item.Default[corev1.ResourceMemory]),
		"default.memory, got %v", item.Default[corev1.ResourceMemory])
}

// TestBuildWorkshopLimitRangeIsAPureFunction pins the SSA precondition every
// object this controller applies must meet: two calls with the same input must
// produce identical objects, so a re-apply is a no-op rather than a rewrite.
func TestBuildWorkshopLimitRangeIsAPureFunction(t *testing.T) {
	assert.Equal(t, BuildWorkshopLimitRange("ws-demo"), BuildWorkshopLimitRange("ws-demo"))
}

// TestWorkshopLimitRangeDefaultsFitTheQuota keeps the two builders honest about
// each other: the per-container defaults must leave room for more than one pod
// inside the quota's own ceilings, or the first test session would exhaust the
// namespace by itself.
func TestWorkshopLimitRangeDefaultsFitTheQuota(t *testing.T) {
	lr := BuildWorkshopLimitRange("ws-demo")
	rq := BuildWorkshopQuota("ws-demo")
	item := lr.Spec.Limits[0]

	cpuLimit := item.Default[corev1.ResourceCPU]
	memLimit := item.Default[corev1.ResourceMemory]
	hardCPU := rq.Spec.Hard[corev1.ResourceLimitsCPU]
	hardMem := rq.Spec.Hard[corev1.ResourceLimitsMemory]
	assert.Less(t, cpuLimit.MilliValue()*2, hardCPU.MilliValue(),
		"two defaulted containers must fit inside limits.cpu")
	assert.Less(t, memLimit.Value()*2, hardMem.Value(),
		"two defaulted containers must fit inside limits.memory")
}
