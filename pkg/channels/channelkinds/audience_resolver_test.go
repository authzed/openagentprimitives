package channelkinds_test

import (
	"context"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeResolver struct {
	cap  channelkinds.Capability
	subs []string
	err  error
}

func (f *fakeResolver) AudienceCapability() channelkinds.Capability { return f.cap }
func (f *fakeResolver) ResolveAudience(ctx context.Context, sess channelkinds.SessionInfo) ([]string, error) {
	return f.subs, f.err
}

func TestCapability_Ordering(t *testing.T) {
	assert.Equal(t, channelkinds.Capability(0), channelkinds.CapabilityUnsupported)
	assert.Less(t, channelkinds.CapabilityUnsupported, channelkinds.CapabilitySingleUser)
	assert.Less(t, channelkinds.CapabilitySingleUser, channelkinds.CapabilityFull)
}

func TestAudienceResolver_Interface(t *testing.T) {
	var r channelkinds.AudienceResolver = &fakeResolver{
		cap:  channelkinds.CapabilityFull,
		subs: []string{"user:alice", "user:bob"},
	}
	assert.Equal(t, channelkinds.CapabilityFull, r.AudienceCapability())
	subs, err := r.ResolveAudience(context.Background(), channelkinds.SessionInfo{})
	require.NoError(t, err)
	assert.Equal(t, []string{"user:alice", "user:bob"}, subs)
}

func TestSessionInfo_ZeroValue(t *testing.T) {
	var s channelkinds.SessionInfo
	assert.Empty(t, s.Namespace)
	assert.Empty(t, s.Name)
	assert.Empty(t, s.SessionInitiator)
	assert.Nil(t, s.Channel)
}
