package toolcheck_test

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// consistencyCapturingSpiceDB records the Consistency each CheckPermission call
// carried, and always answers HAS_PERMISSION so the external slot-grant leg
// resolves in one call.
type consistencyCapturingSpiceDB struct {
	consistencies []*v1.Consistency
}

func (f *consistencyCapturingSpiceDB) CheckPermission(_ context.Context, in *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	f.consistencies = append(f.consistencies, in.Consistency)
	return &v1.CheckPermissionResponse{
		Permissionship: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		CheckedAt:      &v1.ZedToken{Token: "zt-new"},
	}, nil
}

// TestCheckExternalSlotGrant_RequireFreshestForcesFullyConsistent pins the
// re-prompt-churn fix: the external slot-grant read normally FLOORS on the
// cache's token (AtLeastAsFresh) for latency, but a grant written by ANOTHER
// process (channelsd handling a decision) may post-date that token, so a floored
// read can serve a pre-write snapshot and re-prompt for a call already granted.
// in.RequireFreshest must override the floor with a fully-consistent read, the
// same guarantee the readonly/readwrite path already honors.
func TestCheckExternalSlotGrant_RequireFreshestForcesFullyConsistent(t *testing.T) {
	in := func(freshest bool) authz.Inputs {
		return authz.Inputs{
			Subject:           "u-1",
			Args:              map[string]any{"host": "aa.com"},
			AgentSessionRef:   "default/sess-1",
			SlotResourceTypes: []string{browserschema.WebHostType},
			RequireFreshest:   freshest,
		}
	}

	t.Run("cached token, not freshest: floors at AtLeastAsFresh", func(t *testing.T) {
		cache := toolcheck.NewZedTokenCache()
		cache.Set(browserschema.WebHostType, "aa.com", "zt-cached")
		cli := &consistencyCapturingSpiceDB{}

		res := toolcheck.Checker{Cli: cli, Cache: cache}.CheckToolCall(context.Background(), externalActPerm(), in(false))
		require.Equal(t, authz.OutcomeAllowed, res.Outcome)
		require.Len(t, cli.consistencies, 1)
		assert.NotNil(t, cli.consistencies[0].GetAtLeastAsFresh(),
			"with a cached token and no freshness demand, the read floors at AtLeastAsFresh")
	})

	t.Run("cached token, RequireFreshest: overridden to FullyConsistent", func(t *testing.T) {
		cache := toolcheck.NewZedTokenCache()
		cache.Set(browserschema.WebHostType, "aa.com", "zt-cached")
		cli := &consistencyCapturingSpiceDB{}

		res := toolcheck.Checker{Cli: cli, Cache: cache}.CheckToolCall(context.Background(), externalActPerm(), in(true))
		require.Equal(t, authz.OutcomeAllowed, res.Outcome)
		require.Len(t, cli.consistencies, 1)
		assert.True(t, cli.consistencies[0].GetFullyConsistent(),
			"RequireFreshest must override the cache floor so a channelsd-written grant is visible")
	})
}
