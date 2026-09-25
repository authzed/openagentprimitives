package browser

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestBrowserAudienceCapability(t *testing.T) {
	r := &Kind{}
	assert.Equal(t, channelkinds.CapabilitySingleUser, r.AudienceCapability())
}

func TestBrowserAudienceResolver_HappyPath(t *testing.T) {
	r := &Kind{}
	subs, err := r.ResolveAudience(context.Background(),
		channelkinds.SessionInfo{SessionInitiator: identity.CanonicalFromTrusted("user:operator", "test fixture")})
	require.NoError(t, err)
	assert.Equal(t, []string{"user:operator"}, subs)
}

func TestBrowserAudienceResolver_MissingInitiator(t *testing.T) {
	r := &Kind{}
	_, err := r.ResolveAudience(context.Background(), channelkinds.SessionInfo{})
	require.Error(t, err)
	// The shared clienthosted resolver is parameterised by kind name so the
	// failure names the surface the user is looking at, not "clienthosted".
	assert.Contains(t, err.Error(), KindName)
}
