package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
)

func TestParseRefClassifiesAssertion(t *testing.T) {
	k := &Kind{}
	cases := []struct {
		name     string
		spec     string
		strength pinning.Strength
		wantErr  bool
	}{
		{name: "asserted hash → frozen", spec: "gh@sha256:abc123", strength: pinning.StrengthFrozen},
		{name: "no assertion → unpinned", spec: "gh", strength: pinning.StrengthUnpinned},
		{name: "empty → error", spec: "", wantErr: true},
		{name: "empty hash after @ → error", spec: "gh@", wantErr: true},
		{name: "empty name → error", spec: "@sha256:abc", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := k.ParseRef(tc.spec)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "mcp", ref.Kind)
			assert.Equal(t, tc.strength, ref.Strength)
		})
	}
}

func TestResolveAndVerifyAreSyntactic(t *testing.T) {
	k := &Kind{}
	ref, err := k.ParseRef("gh@sha256:abc123")
	require.NoError(t, err)
	f, err := k.Resolve(context.Background(), ref)
	require.NoError(t, err)
	assert.Equal(t, "sha256:abc123", f.Digest)

	rep, err := k.Verify(context.Background(), ref, pinning.Frozen{Digest: "sha256:abc123"})
	require.NoError(t, err)
	assert.False(t, rep.Drifted)
	rep, err = k.Verify(context.Background(), ref, pinning.Frozen{Digest: "sha256:OLD"})
	require.NoError(t, err)
	assert.True(t, rep.Drifted)
}

func TestKindIsRegistered(t *testing.T) {
	_, ok := registry.Get("mcp")
	assert.True(t, ok)
}
