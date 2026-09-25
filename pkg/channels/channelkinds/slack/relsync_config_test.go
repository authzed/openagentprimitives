package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// TestSlackSyncKind_AsksNothingAndNeedsNoEndpoint: the sync covers whatever
// the bot token reaches, so there is nothing to ask and no endpoint —
// asking anyway would invent a setting the kind does not read.
func TestSlackSyncKind_AsksNothingAndNeedsNoEndpoint(t *testing.T) {
	var k relsync.Configurable = &SyncKind{}
	assert.Empty(t, k.ConfigScreens(relsync.ExistingConfig{}))
	assert.False(t, k.NeedsEndpoint())
	cfg, err := k.BuildConfig(nil)
	require.NoError(t, err)
	assert.Nil(t, cfg)
}
