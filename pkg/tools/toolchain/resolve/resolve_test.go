package resolve

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestToolchainSetDigest_OrderIndependentAndContentSensitive(t *testing.T) {
	a := []spiceboxv1alpha1.ToolchainMount{
		{Name: "go", Image: "img-go@sha256:1"},
		{Name: "node", Image: "img-node@sha256:2"},
	}
	b := []spiceboxv1alpha1.ToolchainMount{
		{Name: "node", Image: "img-node@sha256:2"},
		{Name: "go", Image: "img-go@sha256:1"},
	}
	assert.Equal(t, toolchainSetDigest(a), toolchainSetDigest(b),
		"digest must be order-independent or a reshuffled class churns the PodSpec under SSA")

	c := []spiceboxv1alpha1.ToolchainMount{
		{Name: "go", Image: "img-go@sha256:DIFFERENT"},
		{Name: "node", Image: "img-node@sha256:2"},
	}
	assert.NotEqual(t, toolchainSetDigest(a), toolchainSetDigest(c),
		"a changed image digest must change the set digest")
	assert.Len(t, toolchainSetDigest(a), 64, "hex sha256")
}
