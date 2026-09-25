package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

type fakeSpiceDB struct {
	subjects []string
	err      error
	calls    []string
}

func (f *fakeSpiceDB) LookupSubjects(_ context.Context, subjectRef string) ([]string, error) {
	f.calls = append(f.calls, subjectRef)
	return f.subjects, f.err
}

func TestSlackAudienceCapability(t *testing.T) {
	r := newAudienceResolverForTest(&fakeSpiceDB{})
	assert.Equal(t, channelkinds.CapabilityFull, r.AudienceCapability())
}

func TestSlackResolveAudience_HappyPath(t *testing.T) {
	sdb := &fakeSpiceDB{subjects: []string{"user:alice", "user:bob"}}
	r := newAudienceResolverForTest(sdb)

	sess := channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C123"},
		},
	}
	subs, err := r.ResolveAudience(context.Background(), sess)
	require.NoError(t, err)
	assert.Equal(t, []string{"user:alice", "user:bob"}, subs)

	require.Len(t, sdb.calls, 1)
	assert.Equal(t, "slack_channel:C123#view", sdb.calls[0])
}

func TestSlackResolveAudience_MissingBinding(t *testing.T) {
	r := newAudienceResolverForTest(&fakeSpiceDB{})

	_, err := r.ResolveAudience(context.Background(), channelkinds.SessionInfo{Channel: nil})
	require.Error(t, err)
}

func TestSlackResolveAudience_MissingChannelID(t *testing.T) {
	r := newAudienceResolverForTest(&fakeSpiceDB{})

	sess := channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{}},
	}
	_, err := r.ResolveAudience(context.Background(), sess)
	require.Error(t, err)
}
