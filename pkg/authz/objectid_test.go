package authz

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The git toolkit keys git_repo with [normalize_url, spicedb_escape]
// (toolkits/git.yaml). This is the exact chain the live bug bypassed.
func TestNewObjectID_AppliesTheDeclaredChain(t *testing.T) {
	got, err := NewObjectID("https://github.com/acme/app", []string{"normalize_url", "spicedb_escape"})
	require.NoError(t, err, "the chain is two registered, injective transforms")
	assert.Equal(t, "https=3A//github=2Ecom/acme/app", got.String())
}

func TestNewObjectID_EmptyChainIsIdentity(t *testing.T) {
	got, err := NewObjectID("company-4210", nil)
	require.NoError(t, err)
	assert.Equal(t, "company-4210", got.String(),
		"a slot with no declared transforms uses ids that are already distinct resources")
}

func TestNewObjectID_UnknownTransformErrors(t *testing.T) {
	_, err := NewObjectID("x", []string{"no_such_transform"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no_such_transform")
}

func TestNewObjectID_EmptyResultErrors(t *testing.T) {
	// An empty id names no instance and cannot be revoked by id later, so it
	// must fail at construction rather than become a tuple on "".
	_, err := NewObjectID("", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty object id")
}

func TestObjectID_ZeroValueIsZero(t *testing.T) {
	var zero ObjectID
	assert.True(t, zero.IsZero())
	assert.Equal(t, "", zero.String())
}
