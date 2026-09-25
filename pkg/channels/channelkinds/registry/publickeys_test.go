package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// channelOfKind builds the minimum Channel PublicSecretKeysFor dispatches on.
func channelOfKind(t *testing.T, kind string) *spiceboxv1alpha1.Channel {
	t.Helper()
	return &spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{Kind: kind}}
}

// TestPublicSecretKeysFor_FailsClosed covers the three ways a caller can fail to
// get an answer. All three must report NO public keys AND an error, because the
// empty slice on its own is indistinguishable from a kind that legitimately
// declares nothing — and reading it as "none of this Secret is secret" is the
// single mistake this function must not enable.
func TestPublicSecretKeysFor_FailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		ch      *spiceboxv1alpha1.Channel
		wantErr string
	}{
		{name: "nil Channel: error, no keys", ch: nil, wantErr: "nil Channel"},
		{name: "empty kind: error naming the registered set, no keys",
			ch: channelOfKind(t, ""), wantErr: "unknown kind"},
		{name: "unregistered kind: error naming it, no keys",
			ch: channelOfKind(t, "nosuchkind"), wantErr: "nosuchkind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PublicSecretKeysFor(tc.ch)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, got, "no keys may accompany an error")
		})
	}
}

// TestPublicSecretKeysFor_DispatchesToTheKind proves the forwarding, using a
// stub registered for this test rather than a shipped kind, so the assertion is
// about the dispatch and not about any particular kind's policy.
func TestPublicSecretKeysFor_DispatchesToTheKind(t *testing.T) {
	saved := All()
	t.Cleanup(func() {
		Reset()
		for _, k := range saved {
			Register(k)
		}
	})
	Reset()
	Register(publicKeyStubKind{stubKind{name: "declaring"}})
	Register(stubKind{name: "silent"})

	got, err := PublicSecretKeysFor(channelOfKind(t, "declaring"))
	require.NoError(t, err)
	assert.Equal(t, []string{"public-id"}, got)

	got, err = PublicSecretKeysFor(channelOfKind(t, "silent"))
	require.NoError(t, err, "a kind that declares nothing is not an error")
	assert.Empty(t, got, "declaring nothing means every key of that Secret stays secret")
}

// publicKeyStubKind is stubKind with the one answer this file turns on.
type publicKeyStubKind struct{ stubKind }

func (publicKeyStubKind) PublicSecretKeys(*spiceboxv1alpha1.Channel) []string {
	return []string{"public-id"}
}
