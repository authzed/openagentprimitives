// pkg/platform/identity/setup/store_internal_test.go
//
// White-box tests for buildCredential, which is unexported. store_test.go and
// engine_test.go stay package setup_test (black-box, exercising only the
// exported Store API) — this file lives in package setup instead of
// converting that whole suite, since Go links both variants into one test
// binary for the package and the credkind/imports blank import already
// registered there (engine_test.go) covers this file too.
package setup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCredential_DelegatesToTheKind(t *testing.T) {
	got, err := buildCredential("gh", "static", "demo-agent-gh", "token")
	require.NoError(t, err)
	assert.Equal(t, "static", got.Type)
	require.NotNil(t, got.Static)
	assert.Equal(t, "demo-agent-gh", got.Static.SecretRef.Name)
	assert.Equal(t, "token", got.Static.SecretRef.Key)
}

func TestBuildCredential_UnwritableTypeIsAnError(t *testing.T) {
	_, err := buildCredential("c", "federated", "s", "k")
	require.Error(t, err, "setup cannot write a type that stores nothing")
	assert.Contains(t, err.Error(), "no stored value")
}

func TestBuildCredential_UnregisteredTypeIsAnError(t *testing.T) {
	// The old switch had no default arm either: an unregistered type silently
	// produced a credential with Type set but every block left nil. This pins
	// the registry-based replacement returning a real, type-naming error.
	_, err := buildCredential("c", "nosuch", "s", "k")
	require.Error(t, err, "an unregistered type must error, not silently produce a credential with no block set")
	assert.Contains(t, err.Error(), "nosuch")
}
