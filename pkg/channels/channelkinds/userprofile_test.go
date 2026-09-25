package channelkinds_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// stubProvider is a minimal UserProfileProvider used to assert the interface
// is satisfiable and type-assertion discovery works.
type stubProvider struct{ p userprofile.Profile }

func (stubProvider) ProfileFields() []userprofile.Field {
	return []userprofile.Field{userprofile.FieldDisplayName, userprofile.FieldTitle}
}

func (s stubProvider) FetchProfileByEmail(_ context.Context, _ channelkinds.LookupDeps, email string) (userprofile.Profile, error) {
	if email != "dana@example.com" {
		return userprofile.Profile{}, channelkinds.ErrProfileNotFound
	}
	return s.p, nil
}

func TestUserProfileProviderDiscoveryByTypeAssertion(t *testing.T) {
	var anyKind any = stubProvider{p: userprofile.Profile{Title: "Director of Support"}}

	prov, ok := anyKind.(channelkinds.UserProfileProvider)
	require.True(t, ok, "stubProvider must satisfy UserProfileProvider")

	assert.Equal(t,
		[]userprofile.Field{userprofile.FieldDisplayName, userprofile.FieldTitle},
		prov.ProfileFields())

	got, err := prov.FetchProfileByEmail(context.Background(), channelkinds.LookupDeps{}, "dana@example.com")
	require.NoError(t, err)
	assert.Equal(t, "Director of Support", got.Title)
}

func TestUserProfileProviderMissEmitsSentinel(t *testing.T) {
	prov := stubProvider{}
	_, err := prov.FetchProfileByEmail(context.Background(), channelkinds.LookupDeps{}, "nobody@example.com")
	require.ErrorIs(t, err, channelkinds.ErrProfileNotFound,
		"a miss must be distinguishable from a transport failure")
}

// TestNonProviderKindIsNotDiscovered pins the negative: a kind that does not
// implement the interface must not be mistaken for one.
func TestNonProviderKindIsNotDiscovered(t *testing.T) {
	var notAProvider any = struct{}{}
	_, ok := notAProvider.(channelkinds.UserProfileProvider)
	assert.False(t, ok)
}
