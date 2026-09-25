package onepassword

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// TestOnePasswordSyncKind_NeedsAnEndpointAndAsksNothingElse: the SCIM
// Bridge is customer-hosted, so the endpoint is required and there is no
// default to guess.
func TestOnePasswordSyncKind_NeedsAnEndpointAndAsksNothingElse(t *testing.T) {
	var k relsync.Configurable = &SyncKind{}
	assert.True(t, k.NeedsEndpoint())
	assert.Empty(t, k.ConfigScreens(relsync.ExistingConfig{}))
}
