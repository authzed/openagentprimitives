package externaltoken

import (
	"testing"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
)

func TestCredID_StableAndDistinct(t *testing.T) {
	static := spiceboxv1alpha1.CredentialSource{Type: "static", Namespace: "ids", Name: "sec", Key: "github"}
	other := spiceboxv1alpha1.CredentialSource{Type: "static", Namespace: "ids", Name: "sec", Key: "slack"}

	// Deterministic: same source → same id, twice.
	assert.Equal(t, CredID(static), CredID(static), "CredID must be deterministic")
	// Co-located creds (same Secret, different Key) must NOT collide.
	assert.NotEqual(t, CredID(static), CredID(other), "different data keys must yield different credIDs")
	// Federated distinguishes on Resource even with empty Key.
	fedA := spiceboxv1alpha1.CredentialSource{Type: "federated", Namespace: "ids", Name: "idp", Resource: "https://a.example.com"}
	fedB := spiceboxv1alpha1.CredentialSource{Type: "federated", Namespace: "ids", Name: "idp", Resource: "https://b.example.com"}
	assert.NotEqual(t, CredID(fedA), CredID(fedB), "different federated resources must differ")
}

func TestValueHash_KeyedAndDeterministic(t *testing.T) {
	key := []byte("00000000000000000000000000000000")
	assert.Equal(t, ValueHash(key, "tok"), ValueHash(key, "tok"), "deterministic")
	assert.NotEqual(t, ValueHash(key, "tok"), ValueHash([]byte("11111111111111111111111111111111"), "tok"),
		"different key must change the hash")
	assert.NotEqual(t, ValueHash(key, "tok"), ValueHash(key, "tok2"), "different value must change the hash")
}
