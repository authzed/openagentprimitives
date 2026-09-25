package hooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestPoolDestinationAsksForViewMemoryOnTheResource(t *testing.T) {
	var gotResource, gotPerm string
	d := PoolDestination(func(_ context.Context, resource, permission string) ([]string, error) {
		gotResource, gotPerm = resource, permission
		return []string{"user:ann"}, nil
	}, "dossier", "d-1")

	subs, err := d.Audience(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"user:ann"}, subs)
	assert.Equal(t, "dossier:d-1", gotResource)
	assert.Equal(t, memory.PermissionViewMemory, gotPerm,
		"the audience is who may VIEW the pool, never the slot permission the session holds")
	assert.Equal(t, "dossier:d-1", d.Describe())
}

// A pool can always enumerate its recipients — the permission expansion IS
// the enumeration — so the bypasses written for channels that cannot must
// never engage for it.
func TestPoolDestinationIsAlwaysFullyCapable(t *testing.T) {
	d := PoolDestination(func(context.Context, string, string) ([]string, error) { return nil, nil }, "dossier", "d-1")
	assert.Equal(t, channelkinds.CapabilityFull, d.Capability())
}

// The sharpest failure mode in the whole design: an empty audience passes the
// gate vacuously, so a lookup FAILURE must not become one.
func TestPoolDestinationReportsAnErrorRatherThanAnEmptyAudience(t *testing.T) {
	d := PoolDestination(func(context.Context, string, string) ([]string, error) {
		return nil, assert.AnError
	}, "dossier", "d-1")
	subs, err := d.Audience(context.Background())
	require.Error(t, err)
	assert.Nil(t, subs)
}

// A genuinely empty audience is NOT an error: a resource type declaring no
// view_memory has nobody who can read its pool, which the design rules is
// allowed — the audience derives live, so declaring the permission later
// makes everything already written readable.
func TestPoolDestinationAllowsAGenuinelyEmptyAudience(t *testing.T) {
	d := PoolDestination(func(context.Context, string, string) ([]string, error) {
		return nil, nil
	}, "dossier", "d-1")
	subs, err := d.Audience(context.Background())
	require.NoError(t, err)
	assert.Empty(t, subs)
}

func TestPoolDestinationRefusesAnUnwiredLookup(t *testing.T) {
	d := PoolDestination(nil, "dossier", "d-1")
	_, err := d.Audience(context.Background())
	require.Error(t, err, "no lookup wired is an unknown audience, not an empty one")
}
