package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// TestPublicEndpointSpec_RoundTripsThroughYAML decodes a PublicEndpoint
// manifest and asserts every Spec field lands on the struct field its json
// tag names. sigs.k8s.io/yaml converts through encoding/json, which silently
// drops unrecognized keys rather than erroring — so a misspelled or missing
// json tag would NOT surface as a decode error. Each field below is asserted
// against a distinct literal (no two fields share a value) specifically so a
// mis-wired tag reads back as the wrong field's zero value instead of
// accidentally matching a neighboring assertion.
func TestPublicEndpointSpec_RoundTripsThroughYAML(t *testing.T) {
	const src = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: PublicEndpoint
metadata:
  name: webd
spec:
  target: {namespace: agentprimitives-system, service: spicebox-webd, port: 8080}
  provider: ngrok
  authTokenRef: {namespace: ngrok-secrets-ns, name: ngrok-authtoken, key: token}
`
	var pe PublicEndpoint
	require.NoError(t, yaml.Unmarshal([]byte(src), &pe))

	assert.Equal(t, "ngrok", pe.Spec.Provider)

	assert.Equal(t, "agentprimitives-system", pe.Spec.Target.Namespace,
		"target.namespace must decode onto PublicEndpointTarget.Namespace")
	assert.Equal(t, "spicebox-webd", pe.Spec.Target.Service)
	assert.Equal(t, int32(8080), pe.Spec.Target.Port)

	assert.Equal(t, "ngrok-secrets-ns", pe.Spec.AuthTokenRef.Namespace,
		"the ref must carry a namespace: this CRD is cluster-scoped")
	assert.Equal(t, "ngrok-authtoken", pe.Spec.AuthTokenRef.Name)
	assert.Equal(t, "token", pe.Spec.AuthTokenRef.Key)

	assert.Empty(t, pe.Spec.ReservedDomain, "omitted means the provider assigns a URL per session")
}
