package trustedkeys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequiredKey(t *testing.T) {
	t.Run("ref under the pinned prefix: pinned with the P-256 key", func(t *testing.T) {
		pub, pinned, err := RequiredKey(OAPAgentsPrefix + "demo-agent:1")
		require.NoError(t, err)
		assert.True(t, pinned)
		ec, ok := pub.(*ecdsa.PublicKey)
		require.True(t, ok, "key is %T", pub)
		assert.Equal(t, elliptic.P256(), ec.Curve)
	})
	t.Run("lookalike repo: not pinned, nil key", func(t *testing.T) {
		pub, pinned, err := RequiredKey("ghcr.io/authzed/openagentprimitives-evil/x:1")
		require.NoError(t, err)
		assert.False(t, pinned)
		assert.Nil(t, pub)
	})
	t.Run("ref outside the prefix: not pinned, nil key", func(t *testing.T) {
		pub, pinned, err := RequiredKey("ghcr.io/acme/agents/demo-agent:1")
		require.NoError(t, err)
		assert.False(t, pinned)
		assert.Nil(t, pub)
	})
	for _, ref := range []string{
		"ghcr.io:443/authzed/openagentprimitives/agents/x:1",
		"ghcr.io./authzed/openagentprimitives/agents/x:1",
		"ghcr.io.:443/authzed/openagentprimitives/agents/x:1",
		"GHCR.IO/authzed/openagentprimitives/agents/x:1",
	} {
		t.Run("alternate host spelling "+ref+": still pinned", func(t *testing.T) {
			pub, pinned, err := RequiredKey(ref)
			require.NoError(t, err)
			assert.True(t, pinned, "a spelling the registry client dials as ghcr.io must never escape the pin")
			assert.NotNil(t, pub)
		})
	}
	t.Run("host with no canonical form: error, never silently unpinned", func(t *testing.T) {
		_, _, err := RequiredKey("ghcr.io../authzed/openagentprimitives/agents/x:1")
		require.Error(t, err)
	})
	t.Run("unparseable ref: error", func(t *testing.T) {
		_, pinned, err := RequiredKey("::bad")
		require.Error(t, err)
		assert.False(t, pinned)
	})
	t.Run("embedded key fails to parse: pinned=true with the error (fails closed)", func(t *testing.T) {
		orig := oapAgentsPub
		t.Cleanup(func() { oapAgentsPub = orig })
		oapAgentsPub = []byte("not a pem block")
		pub, pinned, err := RequiredKey(OAPAgentsPrefix + "demo-agent:1")
		require.Error(t, err)
		assert.True(t, pinned, "a caller that drops err must still see the ref as pinned")
		assert.Nil(t, pub)
	})
}
