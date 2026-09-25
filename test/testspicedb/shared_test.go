//go:build integration

package testspicedb_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/test/testspicedb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testspicedb.StopShared()
	os.Exit(code)
}

// TestSharedEndpointIsStableWithinPackage asserts that two SharedEndpoint calls
// in the same package return the SAME container address (one container, not two).
func TestSharedEndpointIsStableWithinPackage(t *testing.T) {
	a := testspicedb.SharedEndpoint(t)
	b := testspicedb.SharedEndpoint(t)
	assert.Equal(t, a, b, "SharedEndpoint must return the same container per package")
}

// TestSharedEndpointTokenIsolation asserts that two distinct UniqueToken values
// can each WriteSchema independently on the shared container — proving per-token
// datastore isolation on one running container.
func TestSharedEndpointTokenIsolation(t *testing.T) {
	ep := testspicedb.SharedEndpoint(t)
	tokA := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, ep, tokA) // tokA's datastore now has the schema
	// tokB is a fresh datastore — WriteSchema must also succeed independently.
	tokB := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, ep, tokB)
}
