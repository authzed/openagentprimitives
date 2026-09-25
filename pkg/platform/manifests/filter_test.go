package manifests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestFilterByInstallTier(t *testing.T) {
	in := []*unstructured.Unstructured{
		{Object: map[string]any{"metadata": map[string]any{"name": "a", "labels": map[string]any{"agentprimitives.authzed.com/install-tier": "workspace-provisioner-rwx"}}}},
		{Object: map[string]any{"metadata": map[string]any{"name": "b"}}},
		{Object: map[string]any{"metadata": map[string]any{"name": "c", "labels": map[string]any{"agentprimitives.authzed.com/install-tier": "workspace-provisioner-rwx"}}}},
	}
	match, rest := FilterByInstallTier(in, "agentprimitives.authzed.com/install-tier", "workspace-provisioner-rwx")
	assert.Len(t, match, 2)
	assert.Len(t, rest, 1)
	assert.Equal(t, "b", rest[0].GetName())
}
