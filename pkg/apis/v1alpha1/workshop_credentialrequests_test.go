package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorkshopCredentialRequests_RoundTrip proves the credentialRequests spec
// and status fields survive a DeepCopy round-trip — which also proves
// mage gen:api regenerated zz_generated.deepcopy.go for the new subtypes
// (plan 5a §6: the sidecar's request_credential tool writes spec, the
// channelsd WorkshopCredentialWatcher writes status).
func TestWorkshopCredentialRequests_RoundTrip(t *testing.T) {
	w := &Workshop{Spec: WorkshopSpec{
		CredentialRequests: []WorkshopCredentialRequest{
			{Identity: "weather-ai", Credential: "api_key", AuthKind: "pat"},
			{Identity: "weather-ai", Credential: "webhook_secret", AuthKind: "static"},
		},
	}, Status: WorkshopStatus{
		CredentialRequests: []WorkshopCredentialRequestStatus{
			{Identity: "weather-ai", Credential: "api_key", NoticeRef: "req-1"},
		},
	}}
	got := w.DeepCopy()
	require.Len(t, got.Spec.CredentialRequests, 2)
	assert.Equal(t, "pat", got.Spec.CredentialRequests[0].AuthKind)
	assert.Equal(t, "req-1", got.Status.CredentialRequests[0].NoticeRef)
}
