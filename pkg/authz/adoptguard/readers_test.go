package adoptguard

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"
)

func TestNewSecretReader_Wires(t *testing.T) {
	r := NewSecretReader(nil, nil, Warn, func(types.NamespacedName) bool { return false })
	assert.NotNil(t, r)
	assert.Equal(t, Warn, r.Mode)
}

func TestNewConfigMapReader_Wires(t *testing.T) {
	r := NewConfigMapReader(nil, nil, Warn, func(types.NamespacedName) bool { return false })
	assert.NotNil(t, r)
	assert.Equal(t, Warn, r.Mode)
}

func TestFixedInfraDefaults_AllowsKnownInfra(t *testing.T) {
	allow := FixedInfraDefaults("agentprimitives-system")
	// Secrets in agentprimitives-system that the operator reads directly
	assert.True(t, allow(types.NamespacedName{Namespace: "agentprimitives-system", Name: "spicebox-nats-identity"}))
	assert.True(t, allow(types.NamespacedName{Namespace: "agentprimitives-system", Name: "spicebox-nats-tls"}))
	// ConfigMap in agentprimitives-system that the operator reads directly
	assert.True(t, allow(types.NamespacedName{Namespace: "agentprimitives-system", Name: "publisher-keys"}))
	// Unrelated object must not be allowlisted
	assert.False(t, allow(types.NamespacedName{Namespace: "default", Name: "random-secret"}))
}
